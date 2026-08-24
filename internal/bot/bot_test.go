package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"niulai-wecom-bot/internal/config"
	"niulai-wecom-bot/internal/state"
	"niulai-wecom-bot/internal/wecom"
)

type fakeSender struct {
	mu               sync.Mutex
	sends            []sendCall
	responds         []respondCall
	respondImages    []respondImageCall
	uploads          []uploadCall
	failSend         bool
	failUpload       bool
	failRespondImage bool
}

type sendCall struct {
	chatID   string
	chatType uint32
	content  string
}

type respondCall struct {
	reqID   string
	content string
}

type respondImageCall struct {
	reqID   string
	mediaID string
}

type uploadCall struct {
	mediaType string
	filename  string
	size      int
}

func (f *fakeSender) SendMarkdown(chatID string, chatType uint32, content string) error {
	if f.failSend {
		return errTest()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, sendCall{chatID: chatID, chatType: chatType, content: content})
	return nil
}

func (f *fakeSender) RespondMarkdown(reqID, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responds = append(f.responds, respondCall{reqID: reqID, content: content})
	return nil
}

func (f *fakeSender) RespondImage(reqID, mediaID string) error {
	if f.failRespondImage {
		return errTest()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respondImages = append(f.respondImages, respondImageCall{reqID: reqID, mediaID: mediaID})
	return nil
}

func (f *fakeSender) UploadMedia(_ context.Context, mediaType, filename string, data []byte) (string, error) {
	if f.failUpload {
		return "", errTest()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, uploadCall{mediaType: mediaType, filename: filename, size: len(data)})
	return "media-id-1", nil
}

func errTest() error {
	return errTestType{}
}

type errTestType struct{}

func (errTestType) Error() string { return "test error" }

// pngBytes 构造带真实 PNG magic 的测试图片数据，可通过 http.DetectContentType 嗅探
func pngBytes(n int) []byte {
	header := []byte("\x89PNG\r\n\x1a\n")
	if n < len(header) {
		n = len(header)
	}
	return append(header, bytes.Repeat([]byte{0x42}, n-len(header))...)
}

// waitFor 轮询等待条件成立，用于断言异步 goroutine 完成的回复
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mustMarshalJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestIsStopCommand(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	nl := New(&config.Config{CooldownMinutes: 1}, nil, logger)

	cases := []struct {
		name string
		body *wecom.MsgCallbackBody
		want bool
	}{
		{
			name: "valid stop command with bot mention",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				AIBotID: "bot-1",
				Text:    wecom.TextBody{Content: "牛来"},
				Mention: []wecom.Mention{{UserID: "bot-1", Type: 1}},
			},
			want: true,
		},
		{
			name: "missing keyword",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				AIBotID: "bot-1",
				Text:    wecom.TextBody{Content: "hello"},
				Mention: []wecom.Mention{{UserID: "bot-1", Type: 1}},
			},
			want: false,
		},
		{
			name: "mention another user still stops on keyword",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				AIBotID: "bot-1",
				Text:    wecom.TextBody{Content: "牛来"},
				Mention: []wecom.Mention{{UserID: "user-2", Type: 1}},
			},
			want: true,
		},
		{
			name: "no mention still stops on keyword",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				AIBotID: "bot-1",
				Text:    wecom.TextBody{Content: "牛来"},
			},
			want: true,
		},
		{
			name: "non-text message",
			body: &wecom.MsgCallbackBody{
				MsgType: "image",
				AIBotID: "bot-1",
				Mention: []wecom.Mention{{UserID: "bot-1", Type: 1}},
			},
			want: false,
		},
		{
			name: "keyword without bot id",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				Text:    wecom.TextBody{Content: "牛来"},
			},
			want: true,
		},
		{
			name: "keyword with spaces",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				AIBotID: "bot-1",
				Text:    wecom.TextBody{Content: "  牛 来  "},
				Mention: []wecom.Mention{{UserID: "bot-1", Type: 1}},
			},
			want: true,
		},
		{
			name: "keyword with full-width and zero-width spaces",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				AIBotID: "bot-1",
				Text:    wecom.TextBody{Content: "　牛​来 "},
				Mention: []wecom.Mention{{UserID: "bot-1", Type: 1}},
			},
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nl.isStopCommand(c.body)
			if got != c.want {
				t.Fatalf("isStopCommand() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCaptureMultipleChatIDs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	nl := New(&config.Config{CooldownMinutes: 1}, nil, logger)

	nl.OnMessage("", &wecom.MsgCallbackBody{
		ChatType: "group",
		ChatID:   "chat-1",
		MsgType:  "text",
		Text:     wecom.TextBody{Content: "hello"},
	})
	nl.OnMessage("", &wecom.MsgCallbackBody{
		ChatType: "group",
		ChatID:   "chat-2",
		MsgType:  "text",
		Text:     wecom.TextBody{Content: "hello"},
	})
	nl.OnEvent(&wecom.EventCallbackBody{ChatType: "group", ChatID: "chat-3"})

	got := nl.targetChatIDs()
	want := map[string]struct{}{"chat-1": {}, "chat-2": {}, "chat-3": {}}
	if len(got) != len(want) {
		t.Fatalf("targetChatIDs = %v, want length %d", got, len(want))
	}
	for _, id := range got {
		if _, ok := want[id]; !ok {
			t.Fatalf("unexpected chatid %q", id)
		}
	}
}

func TestTargetChatIDsMergeConfiguredAndDiscovered(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := &config.Config{
		CooldownMinutes: 1,
		TargetChatID:    "target-chat,target-chat-2",
	}
	nl := New(cfg, nil, logger)

	nl.OnMessage("", &wecom.MsgCallbackBody{
		ChatType: "group",
		ChatID:   "target-chat-2",
		MsgType:  "text",
		Text:     wecom.TextBody{Content: "hello"},
	})
	nl.OnMessage("", &wecom.MsgCallbackBody{
		ChatType: "group",
		ChatID:   "chat-1",
		MsgType:  "text",
		Text:     wecom.TextBody{Content: "hello"},
	})

	got := nl.targetChatIDs()
	want := []string{"target-chat", "target-chat-2", "chat-1"}
	if len(got) != len(want) {
		t.Fatalf("targetChatIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targetChatIDs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if cfg.TargetChatID != "target-chat,target-chat-2,chat-1" {
		t.Fatalf("TargetChatID = %q, want %q", cfg.TargetChatID, "target-chat,target-chat-2,chat-1")
	}
}

func TestConfiguredTargetIsUsedBeforeDiscovery(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	nl := New(&config.Config{
		CooldownMinutes: 1,
		TargetChatID:    "target-chat",
	}, nil, logger)

	// 即使没有收到任何群消息回调，固定目标也必须可直接使用。
	got := nl.targetChatIDs()
	if len(got) != 1 || got[0] != "target-chat" {
		t.Fatalf("targetChatIDs = %v, want [target-chat]", got)
	}

	// 收到同一个群的回调时，不应重复追加该 ID。
	nl.OnMessage("", &wecom.MsgCallbackBody{
		ChatType: "group",
		ChatID:   "target-chat",
		MsgType:  "text",
		Text:     wecom.TextBody{Content: "hello"},
	})
	got = nl.targetChatIDs()
	if len(got) != 1 || got[0] != "target-chat" {
		t.Fatalf("targetChatIDs after duplicate discovery = %v, want [target-chat]", got)
	}
}

func TestStopCommandRepliesDefaultText(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	nl := New(&config.Config{CooldownMinutes: 1}, fake, logger)
	nl.SetClient(fake)

	if !nl.machine.StartScreaming() {
		t.Fatal("failed to start screaming")
	}

	nl.OnMessage("req-1", &wecom.MsgCallbackBody{
		MsgType: "text",
		AIBotID: "bot-1",
		From:    wecom.MsgFrom{UserID: "user-1"},
		ChatID:  "chat-1",
		Text:    wecom.TextBody{Content: "牛来"},
		Mention: []wecom.Mention{{UserID: "bot-1", Type: 1}},
	})

	if nl.machine.Current() != state.COOLDOWN {
		t.Fatalf("state = %v, want COOLDOWN", nl.machine.Current())
	}

	// 完成回复是异步发出的，需要等待
	waitFor(t, "finish reply", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.responds) == 1
	})

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.responds[0].reqID != "req-1" || fake.responds[0].content != config.DefaultFinishReplyText {
		t.Fatalf("unexpected finish reply: %+v", fake.responds[0])
	}
}

func TestStopCommandRepliesConfiguredText(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	nl := New(&config.Config{
		CooldownMinutes: 1,
		FinishReplyType: config.ReplyTypeText,
		FinishReplyText: "收到，牛来下班",
	}, fake, logger)
	nl.SetClient(fake)

	if !nl.machine.StartScreaming() {
		t.Fatal("failed to start screaming")
	}

	nl.OnMessage("req-1", &wecom.MsgCallbackBody{
		MsgType: "text",
		ChatID:  "chat-1",
		Text:    wecom.TextBody{Content: "牛来"},
	})

	waitFor(t, "finish reply", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.responds) == 1
	})

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.responds[0].content != "收到，牛来下班" {
		t.Fatalf("unexpected finish reply: %+v", fake.responds)
	}
}

func TestStopCommandRepliesImageAndCachesMedia(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}

	imageData := pngBytes(1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imageData)
	}))
	defer server.Close()

	nl := New(&config.Config{
		CooldownMinutes:     1,
		FinishReplyType:     config.ReplyTypeImage,
		FinishReplyImageURL: server.URL + "/cow.png",
	}, fake, logger)
	nl.SetClient(fake)

	// 连续两次回复：第二次应命中 media_id 缓存，不再上传
	nl.sendFinishReply("req-1")
	nl.sendFinishReply("req-2")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.uploads) != 1 {
		t.Fatalf("expected one upload, got %+v", fake.uploads)
	}
	if fake.uploads[0].mediaType != "image" || fake.uploads[0].filename != "cow.png" || fake.uploads[0].size != len(imageData) {
		t.Fatalf("unexpected upload: %+v", fake.uploads[0])
	}
	if len(fake.respondImages) != 2 {
		t.Fatalf("expected two image replies, got %+v", fake.respondImages)
	}
	for _, r := range fake.respondImages {
		if r.mediaID != "media-id-1" {
			t.Fatalf("unexpected media id: %+v", r)
		}
	}
	if len(fake.responds) != 0 {
		t.Fatalf("expected no text reply, got %+v", fake.responds)
	}
}

func TestStopCommandImageFallbackToText(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{failUpload: true}
	nl := New(&config.Config{
		CooldownMinutes:     1,
		FinishReplyType:     config.ReplyTypeImage,
		FinishReplyImageURL: "https://example.com/cow.png",
	}, fake, logger)
	nl.SetClient(fake)

	nl.sendFinishReply("req-1")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.respondImages) != 0 {
		t.Fatalf("expected no image reply, got %+v", fake.respondImages)
	}
	if len(fake.responds) != 1 || fake.responds[0].content != config.DefaultFinishReplyText {
		t.Fatalf("expected fallback text reply, got %+v", fake.responds)
	}
}

func TestImageRespondFailureFallsBackAndClearsCache(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{failRespondImage: true}

	imageData := pngBytes(1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(imageData)
	}))
	defer server.Close()

	nl := New(&config.Config{
		CooldownMinutes:     1,
		FinishReplyType:     config.ReplyTypeImage,
		FinishReplyImageURL: server.URL + "/cow.png",
	}, fake, logger)
	nl.SetClient(fake)

	// 图片 respond 失败：回退文本，且缓存的 media_id 必须清除
	nl.sendFinishReply("req-1")

	fake.mu.Lock()
	if len(fake.uploads) != 1 {
		t.Fatalf("expected one upload, got %+v", fake.uploads)
	}
	if len(fake.respondImages) != 0 {
		t.Fatalf("expected no successful image reply, got %+v", fake.respondImages)
	}
	if len(fake.responds) != 1 || fake.responds[0].content != config.DefaultFinishReplyText {
		t.Fatalf("expected fallback text reply, got %+v", fake.responds)
	}
	fake.mu.Unlock()

	nl.mediaMu.Lock()
	cached := len(nl.mediaCache)
	nl.mediaMu.Unlock()
	if cached != 0 {
		t.Fatalf("expected media cache cleared after respond failure, got %d entries", cached)
	}

	// 恢复后再次回复：缓存已清除，应重新上传并成功发出图片
	fake.failRespondImage = false
	nl.sendFinishReply("req-2")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.uploads) != 2 {
		t.Fatalf("expected re-upload after cache cleared, got %+v", fake.uploads)
	}
	if len(fake.respondImages) != 1 || fake.respondImages[0].mediaID != "media-id-1" {
		t.Fatalf("expected image reply after recovery, got %+v", fake.respondImages)
	}
}

func TestFetchImageRejectsOversize(t *testing.T) {
	data := make([]byte, wecom.MaxImageMediaSize+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	}))
	defer server.Close()

	if _, _, err := fetchImage(server.URL + "/big.png"); err == nil {
		t.Fatal("expected error for oversize image")
	}
}

func TestFetchImageRejectsNonImageContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>not an image</html>"))
	}))
	defer server.Close()

	if _, _, err := fetchImage(server.URL + "/cow.png"); err == nil {
		t.Fatal("expected error for non-image content")
	}
}

// TestOnMessageImageReplyOverRealClient 是图片回复的端到端回归测试：
// 停止指令经真实 WS 读循环分发到 OnMessage，图片素材上传的响应帧同样
// 依赖该读循环读取；若回复在读循环上同步执行，上传必然超时回退
func TestOnMessageImageReplyOverRealClient(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	imageData := pngBytes(1024)
	imageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(imageData)
	}))
	defer imageServer.Close()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	respondCh := make(chan wecom.Frame, 1)

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var authReq wecom.Frame
		if err := conn.ReadJSON(&authReq); err != nil {
			return
		}
		if err := conn.WriteJSON(wecom.Frame{Cmd: "aibot_subscribe", ErrCode: 0}); err != nil {
			return
		}

		// 认证后立刻下发一条“牛来”停止指令
		callback := wecom.Frame{
			Cmd:     "aibot_msg_callback",
			Headers: wecom.Headers{ReqID: "req-stop-1"},
			Body: mustMarshalJSON(t, wecom.MsgCallbackBody{
				MsgID:    "m-1",
				ChatID:   "chat-1",
				ChatType: "group",
				From:     wecom.MsgFrom{UserID: "user-1"},
				MsgType:  "text",
				Text:     wecom.TextBody{Content: "牛来"},
			}),
		}
		if err := conn.WriteJSON(callback); err != nil {
			return
		}

		for {
			var f wecom.Frame
			if err := conn.ReadJSON(&f); err != nil {
				return
			}
			respond := func(body any) {
				_ = conn.WriteJSON(wecom.Frame{
					Headers: wecom.Headers{ReqID: f.Headers.ReqID},
					Body:    mustMarshalJSON(t, body),
				})
			}
			switch f.Cmd {
			case "aibot_upload_media_init":
				respond(wecom.UploadMediaInitResp{UploadID: "upload-1"})
			case "aibot_upload_media_chunk":
				respond(struct{}{})
			case "aibot_upload_media_finish":
				respond(wecom.UploadMediaFinishResp{MediaID: "media-final", Type: "image"})
			case "aibot_respond_msg":
				select {
				case respondCh <- f:
				default:
				}
			}
		}
	}))
	defer wsServer.Close()

	nl := New(&config.Config{
		CooldownMinutes:     1,
		FinishReplyType:     config.ReplyTypeImage,
		FinishReplyImageURL: imageServer.URL + "/cow.png",
	}, nil, logger)

	client := wecom.NewClient("bot-id", "secret", nl, logger).
		SetURL("ws" + strings.TrimPrefix(wsServer.URL, "http"))
	nl.SetClient(client)

	if !nl.machine.StartScreaming() {
		t.Fatal("failed to start screaming")
	}

	ctx, cancel := context.WithCancel(context.Background())
	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = client.Run(ctx)
	}()
	defer func() {
		cancel()
		runWG.Wait()
	}()

	var frame wecom.Frame
	select {
	case frame = <-respondCh:
	case <-time.After(10 * time.Second):
		t.Fatal("did not receive image reply in time (read loop blocked?)")
	}

	if frame.Headers.ReqID != "req-stop-1" {
		t.Fatalf("respond req_id = %q, want req-stop-1", frame.Headers.ReqID)
	}
	var body wecom.RespondMsgBody
	if err := json.Unmarshal(frame.Body, &body); err != nil {
		t.Fatalf("unmarshal respond body: %v", err)
	}
	if body.MsgType != "image" || body.Image == nil || body.Image.MediaID != "media-final" {
		t.Fatalf("unexpected respond body: %+v", body)
	}
}

func TestStopCommandReadsLocalImageFile(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}

	imagePath := filepath.Join(t.TempDir(), "cow.png")
	if err := os.WriteFile(imagePath, bytes.Repeat([]byte{0x7F}, 2048), 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}

	nl := New(&config.Config{
		CooldownMinutes:     1,
		FinishReplyType:     config.ReplyTypeImage,
		FinishReplyImageURL: imagePath,
	}, fake, logger)
	nl.SetClient(fake)

	nl.sendFinishReply("req-1")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.uploads) != 1 || fake.uploads[0].filename != "cow.png" || fake.uploads[0].size != 2048 {
		t.Fatalf("unexpected upload: %+v", fake.uploads)
	}
	if len(fake.respondImages) != 1 {
		t.Fatalf("expected one image reply, got %+v", fake.respondImages)
	}
}

func TestNoFinishReplyWhenStopIgnored(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	nl := New(&config.Config{CooldownMinutes: 1}, fake, logger)
	nl.SetClient(fake)

	// 未处于 SCREAMING 时收到停止指令，不应回复
	nl.OnMessage("req-1", &wecom.MsgCallbackBody{
		MsgType: "text",
		ChatID:  "chat-1",
		Text:    wecom.TextBody{Content: "牛来"},
	})

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.responds) != 0 || len(fake.respondImages) != 0 {
		t.Fatalf("expected no reply, got text=%+v image=%+v", fake.responds, fake.respondImages)
	}
}

func TestStopCommandStartsCooldownOnKeywordWithoutMention(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	nl := New(&config.Config{CooldownMinutes: 1}, nil, logger)

	if !nl.machine.StartScreaming() {
		t.Fatal("failed to start screaming")
	}

	nl.OnMessage("req-1", &wecom.MsgCallbackBody{
		MsgType: "text",
		ChatID:  "chat-1",
		Text:    wecom.TextBody{Content: "牛来"},
	})

	if nl.machine.Current() != state.COOLDOWN {
		t.Fatalf("state = %v, want %v", nl.machine.Current(), state.COOLDOWN)
	}
}

func TestScreamLoopSendsToMultipleChats(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	cfg := &config.Config{
		CooldownMinutes:    1,
		MinIntervalSeconds: 600,
		MaxIntervalSeconds: 600,
	}
	nl := New(cfg, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")
	nl.captureChatID("group", "chat-2")

	ctx, cancel := context.WithCancel(context.Background())
	nl.ctx, nl.cancel = ctx, cancel

	if !nl.machine.StartScreaming() {
		t.Fatal("failed to start screaming")
	}

	screamCtx, screamCancel := context.WithCancel(nl.ctx)

	nl.wg.Add(1)
	go func() {
		defer nl.wg.Done()
		nl.screamLoop(screamCtx)
	}()

	time.Sleep(50 * time.Millisecond)
	screamCancel()

	done := make(chan struct{})
	go func() {
		nl.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("screamLoop did not stop promptly")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sends) == 0 {
		t.Fatal("expected at least one scream message")
	}
	seen := make(map[string]struct{})
	for _, c := range fake.sends {
		seen[c.chatID] = struct{}{}
		if c.content != screamContent {
			t.Fatalf("unexpected content: %q", c.content)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("expected messages to both chats, got chats: %v", seen)
	}
}

func TestScreamLoopUsesTargetChatID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	cfg := &config.Config{
		CooldownMinutes:    1,
		MinIntervalSeconds: 1,
		MaxIntervalSeconds: 1,
		TargetChatID:       "target-chat",
	}
	nl := New(cfg, fake, logger)
	nl.SetClient(fake)

	ctx, cancel := context.WithCancel(context.Background())
	nl.ctx, nl.cancel = ctx, cancel

	if !nl.machine.StartScreaming() {
		t.Fatal("failed to start screaming")
	}

	screamCtx, screamCancel := context.WithCancel(nl.ctx)

	nl.wg.Add(1)
	go func() {
		defer nl.wg.Done()
		nl.screamLoop(screamCtx)
	}()

	time.Sleep(150 * time.Millisecond)
	screamCancel()
	nl.wg.Wait()

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, c := range fake.sends {
		if c.chatID != "target-chat" {
			t.Fatalf("unexpected chatID: %q", c.chatID)
		}
		if c.chatType != wecom.ChatTypeGroup {
			t.Fatalf("unexpected chatType: %d", c.chatType)
		}
		if c.content != screamContent {
			t.Fatalf("unexpected content: %q", c.content)
		}
	}
}

func TestFailureEviction(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{failSend: true}
	cfg := &config.Config{
		CooldownMinutes:    1,
		MinIntervalSeconds: 1,
		MaxIntervalSeconds: 1,
		MaxSendFailures:    2,
	}
	nl := New(cfg, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")
	nl.captureChatID("group", "chat-2")

	ctx, cancel := context.WithCancel(context.Background())
	nl.ctx, nl.cancel = ctx, cancel

	if !nl.machine.StartScreaming() {
		t.Fatal("failed to start screaming")
	}

	screamCtx, screamCancel := context.WithCancel(nl.ctx)

	nl.wg.Add(1)
	go func() {
		defer nl.wg.Done()
		nl.screamLoop(screamCtx)
	}()

	time.Sleep(1100 * time.Millisecond)
	screamCancel()
	nl.wg.Wait()

	// chat-1 应该因连续失败 2 次被移除
	nl.chatsMu.RLock()
	_, ok := nl.chats["chat-1"]
	nl.chatsMu.RUnlock()
	if ok {
		t.Fatal("expected chat-1 to be evicted after failures")
	}
	if got := nl.targetChatIDs(); len(got) != 0 {
		t.Fatalf("targetChatIDs after eviction = %v, want []", got)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sends) != 0 {
		t.Fatal("expected all sends to fail")
	}
}

func TestFailureResetOnSuccess(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 3}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	nl.sendScreamToTargets(context.Background())
	if nl.getSendFailures("chat-1") != 0 {
		t.Fatalf("expected failures reset to 0, got %d", nl.getSendFailures("chat-1"))
	}
}

func TestConfiguredTargetTracksFailures(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{failSend: true}
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 3, TargetChatID: "target-chat"}, fake, logger)
	nl.SetClient(fake)

	for i := 0; i < 3; i++ {
		nl.sendScreamToTargets(context.Background())
	}

	if got := nl.targetChatIDs(); len(got) != 0 {
		t.Fatalf("targetChatIDs after eviction = %v, want []", got)
	}
	if got := nl.getSendFailures("target-chat"); got != 0 {
		t.Fatalf("configured target failures after eviction = %d, want 0", got)
	}
}

func TestDailyTriggerTracking(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 3}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	if !nl.hasPendingChatToday() {
		t.Fatal("expected pending before any trigger")
	}

	nl.sendScreamToTargets(context.Background())
	if nl.hasPendingChatToday() {
		t.Fatal("expected no pending after successful send")
	}

	// 新发现的群聊当天未触发，应重新进入待触发状态
	nl.captureChatID("group", "chat-2")
	if !nl.hasPendingChatToday() {
		t.Fatal("expected pending for newly discovered chat")
	}
}

func TestDailyTriggerTrackingResetsAcrossDays(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 3}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	current := time.Date(2026, 8, 24, 10, 0, 0, 0, time.Local)
	nl.now = func() time.Time { return current }

	nl.sendScreamToTargets(context.Background())
	if nl.hasPendingChatToday() {
		t.Fatal("expected no pending after successful send")
	}

	// 跨天后同群重新进入待触发
	current = current.AddDate(0, 0, 1)
	if !nl.hasPendingChatToday() {
		t.Fatal("expected pending on the next day")
	}
}

func TestFailedSendDoesNotMarkTriggered(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{failSend: true}
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 99}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	nl.sendScreamToTargets(context.Background())
	if !nl.hasPendingChatToday() {
		t.Fatal("expected pending when send fails")
	}
}

func TestStartIsIdempotent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	nl := New(&config.Config{CooldownMinutes: 1}, nil, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	nl.Start(ctx)
	nl.Start(ctx) // 第二次应被忽略，不应 panic 或泄漏

	// 给调度器一点时间启动
	time.Sleep(50 * time.Millisecond)
	nl.Stop()
	nl.Stop() // 重复 Stop 应安全
}
