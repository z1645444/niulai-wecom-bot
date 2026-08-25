package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	sendVoices       []sendVoiceCall
	responds         []respondCall
	respondImages    []respondImageCall
	uploads          []uploadCall
	failSend         bool
	failSendVoice    bool
	failUpload       bool
	failRespondImage bool
}

type sendCall struct {
	chatID   string
	chatType uint32
	content  string
}

type sendVoiceCall struct {
	chatID   string
	chatType uint32
	mediaID  string
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

func (f *fakeSender) SendMarkdown(_ context.Context, chatID string, chatType uint32, content string) error {
	if f.failSend {
		return errTest()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, sendCall{chatID: chatID, chatType: chatType, content: content})
	return nil
}

func (f *fakeSender) SendVoice(_ context.Context, chatID string, chatType uint32, mediaID string) error {
	if f.failSendVoice {
		return errTest()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendVoices = append(f.sendVoices, sendVoiceCall{chatID: chatID, chatType: chatType, mediaID: mediaID})
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
		{
			name: "leading mention with negation does not stop",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				AIBotID: "bot-1",
				Text:    wecom.TextBody{Content: "@牛来 不要停"},
				Mention: []wecom.Mention{{UserID: "bot-1", Type: 1}},
			},
			want: false,
		},
		{
			name: "leading mention only does not stop",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				AIBotID: "bot-1",
				Text:    wecom.TextBody{Content: "@牛来"},
				Mention: []wecom.Mention{{UserID: "bot-1", Type: 1}},
			},
			want: false,
		},
		{
			name: "keyword after leading mention stops",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				AIBotID: "bot-1",
				Text:    wecom.TextBody{Content: "@牛来 牛来"},
				Mention: []wecom.Mention{{UserID: "bot-1", Type: 1}},
			},
			want: true,
		},
		{
			name: "keyword not after mention still stops",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				AIBotID: "bot-1",
				Text:    wecom.TextBody{Content: "快停下 牛来"},
			},
			want: true,
		},
		{
			name: "renamed bot mention with negation does not stop",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				Text:    wecom.TextBody{Content: "@新名字 不要停"},
			},
			want: false,
		},
		{
			name: "consecutive leading mentions are stripped",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				Text:    wecom.TextBody{Content: "@张三 @牛来 不要停"},
			},
			want: false,
		},
		{
			name: "mention without separator does not stop",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				Text:    wecom.TextBody{Content: "@牛来不要停"},
			},
			want: false,
		},
		{
			name: "mention with separator but no content does not stop",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				Text:    wecom.TextBody{Content: "@牛来 "},
			},
			want: false,
		},
		{
			name: "other user mention then keyword stops",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				Text:    wecom.TextBody{Content: "@张三 牛来"},
			},
			want: true,
		},
		{
			name: "mention in middle is not stripped",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				Text:    wecom.TextBody{Content: "牛来@牛来 不要停"},
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

func TestIsStopCommandWithCustomKeyword(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	nl := New(&config.Config{CooldownMinutes: 1, StopKeyword: "牛哥"}, nil, logger)

	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"custom keyword stops", "牛哥", true},
		{"custom keyword after mention stops", "@牛哥 牛哥", true},
		{"custom keyword mention only does not stop", "@牛哥", false},
		{"default keyword does not stop", "牛来", false},
		{"default-name mention stripped generically", "@牛来 不要停", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := &wecom.MsgCallbackBody{
				MsgType: "text",
				Text:    wecom.TextBody{Content: c.content},
			}
			if got := nl.isStopCommand(body); got != c.want {
				t.Fatalf("isStopCommand(%q) = %v, want %v", c.content, got, c.want)
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

	session := nl.sessionFor("chat-1")
	if !session.machine.StartScreaming() {
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

	if session.machine.Current() != state.COOLDOWN {
		t.Fatalf("state = %v, want COOLDOWN", session.machine.Current())
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

	if !nl.sessionFor("chat-1").machine.StartScreaming() {
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

	if !nl.sessionFor("chat-1").machine.StartScreaming() {
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

	session := nl.sessionFor("chat-1")
	if !session.machine.StartScreaming() {
		t.Fatal("failed to start screaming")
	}

	nl.OnMessage("req-1", &wecom.MsgCallbackBody{
		MsgType: "text",
		ChatID:  "chat-1",
		Text:    wecom.TextBody{Content: "牛来"},
	})

	if session.machine.Current() != state.COOLDOWN {
		t.Fatalf("state = %v, want %v", session.machine.Current(), state.COOLDOWN)
	}
}

func TestScreamLoopsRunPerChat(t *testing.T) {
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

	for _, chatID := range []string{"chat-1", "chat-2"} {
		session := nl.sessionFor(chatID)
		if !session.machine.StartScreaming() {
			t.Fatalf("failed to start screaming for %s", chatID)
		}
		screamCtx := session.startScreamCtx(nl.ctx)

		nl.wg.Add(1)
		go func(session *chatSession, chatID string) {
			defer nl.wg.Done()
			nl.screamLoop(screamCtx, session, chatID)
		}(session, chatID)
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() {
		nl.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("screamLoops did not stop promptly")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sends) == 0 {
		t.Fatal("expected at least one scream message")
	}
	seen := make(map[string]struct{})
	for _, c := range fake.sends {
		seen[c.chatID] = struct{}{}
		if c.content != config.DefaultScreamContent {
			t.Fatalf("unexpected content: %q", c.content)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("expected messages to both chats, got chats: %v", seen)
	}
}

// TestStopCommandOnlyStopsOriginatingChat 是会话隔离的回归测试：
// 一个群收到停止指令后，只有该群停止并进入冷却，其他群继续发送
func TestStopCommandOnlyStopsOriginatingChat(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	cfg := &config.Config{
		CooldownMinutes:    1,
		MinIntervalSeconds: 600,
		MaxIntervalSeconds: 600,
	}
	nl := New(cfg, fake, logger)
	nl.SetClient(fake)
	// 加快发送节奏，避免测试受真实间隔影响
	nl.sleep = func(ctx context.Context, _ time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
			return true
		}
	}
	nl.captureChatID("group", "chat-1")
	nl.captureChatID("group", "chat-2")

	ctx, cancel := context.WithCancel(context.Background())
	nl.ctx, nl.cancel = ctx, cancel
	defer func() {
		cancel()
		done := make(chan struct{})
		go func() {
			nl.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("scream loops did not stop after cancel")
		}
	}()

	nl.onTrigger("chat-1")
	nl.onTrigger("chat-2")

	// 等两个群都至少发出一条
	waitFor(t, "initial screams from both chats", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		seen := make(map[string]bool)
		for _, c := range fake.sends {
			seen[c.chatID] = true
		}
		return seen["chat-1"] && seen["chat-2"]
	})

	// chat-1 收到停止指令
	nl.OnMessage("req-1", &wecom.MsgCallbackBody{
		MsgType: "text",
		ChatID:  "chat-1",
		Text:    wecom.TextBody{Content: "牛来"},
	})

	if got := nl.sessionFor("chat-1").machine.Current(); got != state.COOLDOWN {
		t.Fatalf("chat-1 state = %v, want COOLDOWN", got)
	}
	if got := nl.sessionFor("chat-2").machine.Current(); got != state.SCREAMING {
		t.Fatalf("chat-2 state = %v, want SCREAMING", got)
	}

	counts := func() (int, int) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		var c1, c2 int
		for _, c := range fake.sends {
			switch c.chatID {
			case "chat-1":
				c1++
			case "chat-2":
				c2++
			}
		}
		return c1, c2
	}

	// 给 chat-1 的循环留出退出窗口后采样：chat-1 不得再增长，chat-2 必须继续增长
	time.Sleep(50 * time.Millisecond)
	before1, before2 := counts()
	time.Sleep(150 * time.Millisecond)
	after1, after2 := counts()

	if after1 != before1 {
		t.Fatalf("chat-1 kept sending after stop: before=%d after=%d", before1, after1)
	}
	if after2 <= before2 {
		t.Fatalf("chat-2 stopped sending after chat-1 stop: before=%d after=%d", before2, after2)
	}
}

// TestOnTriggerSkipsNonTargetChat 是触发与驱逐交错的回归测试：
// onTrigger 不得为已不在目标列表中的群创建会话、启动发送循环，
// 否则该循环无法再被失败驱逐终止，会一直泄漏到进程重启
func TestOnTriggerSkipsNonTargetChat(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	cfg := &config.Config{
		CooldownMinutes:    1,
		MinIntervalSeconds: 600,
		MaxIntervalSeconds: 600,
	}
	nl := New(cfg, fake, logger)
	nl.SetClient(fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nl.ctx, nl.cancel = ctx, cancel

	nl.onTrigger("ghost-chat")

	if s := nl.findSession("ghost-chat"); s != nil {
		t.Fatal("expected no session created for non-target chat")
	}

	// 若循环被错误启动，第一条喊话会立即发出
	time.Sleep(50 * time.Millisecond)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sends) != 0 {
		t.Fatalf("expected no sends to non-target chat, got %d", len(fake.sends))
	}
}

// TestRediscoveredChatGetsFreshSession 验证约束：被失败驱逐的群重新发现后，
// 以全新的 IDLE 会话参与触发，而不是沿用被驱逐时的会话
func TestRediscoveredChatGetsFreshSession(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{failSend: true}
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 2}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	// 模拟事件进行中被驱逐：会话处于 SCREAMING 时连续失败达到上限
	evicted := nl.sessionFor("chat-1")
	if !evicted.machine.StartScreaming() {
		t.Fatal("failed to start screaming")
	}
	nl.sendScream("chat-1")
	nl.sendScream("chat-1")

	if got := nl.targetChatIDs(); len(got) != 0 {
		t.Fatalf("expected chat evicted, targets = %v", got)
	}
	if s := nl.findSession("chat-1"); s != nil {
		t.Fatal("expected session removed after eviction")
	}

	nl.captureChatID("group", "chat-1")

	if got := nl.targetChatIDs(); len(got) != 1 {
		t.Fatalf("expected chat rediscovered, targets = %v", got)
	}
	fresh := nl.sessionFor("chat-1")
	if fresh == evicted {
		t.Fatal("expected a brand-new session after rediscovery")
	}
	if got := fresh.machine.Current(); got != state.IDLE {
		t.Fatalf("state = %v, want IDLE", got)
	}
}

// TestStopCommandIgnoredWithoutSession 验证没有会话的群收到停止指令时
// 直接忽略：不创建会话，也不回复完成消息
func TestStopCommandIgnoredWithoutSession(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	nl := New(&config.Config{CooldownMinutes: 1}, fake, logger)
	nl.SetClient(fake)

	nl.OnMessage("req-1", &wecom.MsgCallbackBody{
		MsgType: "text",
		ChatID:  "chat-1",
		Text:    wecom.TextBody{Content: "牛来"},
	})

	if s := nl.findSession("chat-1"); s != nil {
		t.Fatal("stop command must not create a session")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.responds) != 0 || len(fake.respondImages) != 0 {
		t.Fatalf("expected no finish reply, got responds=%v images=%v", fake.responds, fake.respondImages)
	}
}

func TestScreamLoopSendsToOwnChat(t *testing.T) {
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

	session := nl.sessionFor("target-chat")
	if !session.machine.StartScreaming() {
		t.Fatal("failed to start screaming")
	}

	screamCtx, screamCancel := context.WithCancel(nl.ctx)

	nl.wg.Add(1)
	go func() {
		defer nl.wg.Done()
		nl.screamLoop(screamCtx, session, "target-chat")
	}()

	time.Sleep(150 * time.Millisecond)
	screamCancel()
	nl.wg.Wait()

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sends) == 0 {
		t.Fatal("expected at least one scream message")
	}
	for _, c := range fake.sends {
		if c.chatID != "target-chat" {
			t.Fatalf("unexpected chatID: %q", c.chatID)
		}
		if c.chatType != wecom.ChatTypeGroup {
			t.Fatalf("unexpected chatType: %d", c.chatType)
		}
		if c.content != config.DefaultScreamContent {
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
	defer cancel()
	nl.ctx, nl.cancel = ctx, cancel

	nl.onTrigger("chat-1")
	nl.onTrigger("chat-2")

	// 两个群各自连续失败 2 次后都应被移出目标列表
	waitFor(t, "eviction of both chats", func() bool {
		return len(nl.targetChatIDs()) == 0
	})

	// 会话随驱逐终止，循环应自行退出
	done := make(chan struct{})
	go func() {
		nl.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scream loops did not stop after eviction")
	}

	nl.chatsMu.RLock()
	_, ok1 := nl.chats["chat-1"]
	_, ok2 := nl.chats["chat-2"]
	nl.chatsMu.RUnlock()
	if ok1 || ok2 {
		t.Fatal("expected both chats to be evicted after failures")
	}
	if s := nl.findSession("chat-1"); s != nil {
		t.Fatal("expected chat-1 session removed after eviction")
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

	nl.sendScream("chat-1")
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
		nl.sendScream("target-chat")
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

	if !nl.hasPendingChat("chat-1") {
		t.Fatal("expected pending before any trigger")
	}

	nl.sendScream("chat-1")
	if nl.hasPendingChat("chat-1") {
		t.Fatal("expected no pending after successful send")
	}

	// 新发现的群聊当天未触发，仍处于待触发；已触发的群不受影响
	nl.captureChatID("group", "chat-2")
	if !nl.hasPendingChat("chat-2") {
		t.Fatal("expected pending for newly discovered chat")
	}
	if nl.hasPendingChat("chat-1") {
		t.Fatal("expected chat-1 to remain triggered")
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

	nl.sendScream("chat-1")
	if nl.hasPendingChat("chat-1") {
		t.Fatal("expected no pending after successful send")
	}

	// 跨天后同群重新进入待触发
	current = current.AddDate(0, 0, 1)
	if !nl.hasPendingChat("chat-1") {
		t.Fatal("expected pending on the next day")
	}
}

func TestFailedSendDoesNotMarkTriggered(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{failSend: true}
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 99}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	nl.sendScream("chat-1")
	if !nl.hasPendingChat("chat-1") {
		t.Fatal("expected pending when send fails")
	}
}

// writeVoiceFile 在临时目录写入语音文件并返回其绝对路径
func writeVoiceFile(t *testing.T) string {
	t.Helper()
	addr := filepath.Join(t.TempDir(), "scream.amr")
	if err := os.WriteFile(addr, []byte("#!AMR\n voice-data"), 0o600); err != nil {
		t.Fatalf("write voice file: %v", err)
	}
	return addr
}

func TestScreamSendsVoiceWhenConfigured(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	addr := writeVoiceFile(t)
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 3, ScreamVoiceFile: addr}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	nl.sendScream("chat-1")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sendVoices) != 1 {
		t.Fatalf("expected 1 voice send, got %d", len(fake.sendVoices))
	}
	voice := fake.sendVoices[0]
	if voice.chatID != "chat-1" || voice.chatType != wecom.ChatTypeGroup || voice.mediaID != "media-id-1" {
		t.Fatalf("unexpected voice send: %+v", voice)
	}
	if len(fake.uploads) != 1 || fake.uploads[0].mediaType != "voice" || fake.uploads[0].filename != "scream.amr" {
		t.Fatalf("unexpected uploads: %+v", fake.uploads)
	}
	if len(fake.sends) != 0 {
		t.Fatalf("expected no text fallback, got %d sends", len(fake.sends))
	}
	if nl.hasPendingChat("chat-1") {
		t.Fatal("expected voice scream to mark the chat triggered")
	}
}

func TestScreamVoiceCachesMediaID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	addr := writeVoiceFile(t)
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 3, ScreamVoiceFile: addr}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	nl.sendScream("chat-1")
	nl.sendScream("chat-1")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sendVoices) != 2 {
		t.Fatalf("expected 2 voice sends, got %d", len(fake.sendVoices))
	}
	if len(fake.uploads) != 1 {
		t.Fatalf("expected voice file uploaded once, got %d", len(fake.uploads))
	}
}

// TestScreamVoiceReuploadsAfterRefreshThreshold 验证缓存的 media_id 超过刷新阈值后
// 自动重新上传，避免用到已过 3 天有效期的 media_id
func TestScreamVoiceReuploadsAfterRefreshThreshold(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	addr := writeVoiceFile(t)
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 3, ScreamVoiceFile: addr}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	current := time.Date(2026, 8, 25, 10, 0, 0, 0, time.Local)
	nl.now = func() time.Time { return current }

	nl.sendScream("chat-1")

	// 阈值之前：命中缓存，不重新上传
	current = current.Add(mediaIDRefreshAfter - time.Hour)
	nl.sendScream("chat-1")

	fake.mu.Lock()
	if len(fake.uploads) != 1 {
		t.Fatalf("expected no re-upload before threshold, got %d", len(fake.uploads))
	}
	fake.mu.Unlock()

	// 超过阈值：缓存视为过期，重新上传
	current = current.Add(2 * time.Hour)
	nl.sendScream("chat-1")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.uploads) != 2 {
		t.Fatalf("expected re-upload after threshold, got %d uploads", len(fake.uploads))
	}
	if len(fake.sendVoices) != 3 {
		t.Fatalf("expected 3 voice sends, got %d", len(fake.sendVoices))
	}
}

// TestFinishReplyImageReuploadsAfterRefreshThreshold 验证图片回复缓存的 media_id
// 超过刷新阈值后同样重新上传
func TestFinishReplyImageReuploadsAfterRefreshThreshold(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}

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

	current := time.Date(2026, 8, 25, 10, 0, 0, 0, time.Local)
	nl.now = func() time.Time { return current }

	nl.sendFinishReply("req-1")

	current = current.Add(mediaIDRefreshAfter + time.Hour)
	nl.sendFinishReply("req-2")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.uploads) != 2 {
		t.Fatalf("expected re-upload after threshold, got %d uploads", len(fake.uploads))
	}
	if len(fake.respondImages) != 2 {
		t.Fatalf("expected 2 image replies, got %d", len(fake.respondImages))
	}
}

func TestScreamVoiceMissingFileFallsBackToText(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	missing := filepath.Join(t.TempDir(), "missing.amr")
	nl := New(&config.Config{
		CooldownMinutes: 1,
		MaxSendFailures: 3,
		ScreamContent:   "妈妈",
		ScreamVoiceFile: missing,
	}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	nl.sendScream("chat-1")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sendVoices) != 0 || len(fake.uploads) != 0 {
		t.Fatalf("expected no voice send or upload, got voices=%d uploads=%d", len(fake.sendVoices), len(fake.uploads))
	}
	if len(fake.sends) != 1 || fake.sends[0].content != "妈妈" {
		t.Fatalf("expected text fallback with scream content, got %+v", fake.sends)
	}
	if nl.hasPendingChat("chat-1") {
		t.Fatal("expected text fallback to mark the chat triggered")
	}
}

func TestScreamVoiceUploadFailureFallsBackToText(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{failUpload: true}
	addr := writeVoiceFile(t)
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 3, ScreamVoiceFile: addr}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	nl.sendScream("chat-1")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sendVoices) != 0 {
		t.Fatalf("expected no voice send, got %d", len(fake.sendVoices))
	}
	if len(fake.sends) != 1 {
		t.Fatalf("expected text fallback, got %d sends", len(fake.sends))
	}
	if got := nl.getSendFailures("chat-1"); got != 0 {
		t.Fatalf("expected no send failure recorded after successful fallback, got %d", got)
	}
}

// TestScreamVoiceOversizeFallsBackToText 验证语音文件超过企业微信 2MB 上限时
// 不发起上传，直接回退为文字喊话
func TestScreamVoiceOversizeFallsBackToText(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	addr := filepath.Join(t.TempDir(), "scream.amr")
	if err := os.WriteFile(addr, make([]byte, wecom.MaxVoiceMediaSize+1), 0o600); err != nil {
		t.Fatalf("write oversize voice file: %v", err)
	}
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 3, ScreamVoiceFile: addr}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	nl.sendScream("chat-1")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.uploads) != 0 || len(fake.sendVoices) != 0 {
		t.Fatalf("expected no upload or voice send, got uploads=%d voices=%d", len(fake.uploads), len(fake.sendVoices))
	}
	if len(fake.sends) != 1 {
		t.Fatalf("expected text fallback, got %d sends", len(fake.sends))
	}
}

func TestScreamVoiceSendFailureFallsBackToTextAndClearsCache(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	addr := writeVoiceFile(t)
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 3, ScreamVoiceFile: addr}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	nl.sendScream("chat-1")

	fake.mu.Lock()
	if len(fake.uploads) != 1 {
		t.Fatalf("expected initial upload, got %d", len(fake.uploads))
	}
	fake.failSendVoice = true
	fake.mu.Unlock()

	nl.sendScream("chat-1")

	fake.mu.Lock()
	if len(fake.sends) != 1 {
		t.Fatalf("expected text fallback after voice send failure, got %d sends", len(fake.sends))
	}
	// 缓存的 media_id 已清除，恢复后重新上传
	fake.failSendVoice = false
	fake.mu.Unlock()

	nl.sendScream("chat-1")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.uploads) != 2 {
		t.Fatalf("expected re-upload after cache cleared, got %d uploads", len(fake.uploads))
	}
	if len(fake.sendVoices) != 2 {
		t.Fatalf("expected 2 voice sends, got %d", len(fake.sendVoices))
	}
}

// TestScreamVoicePausedAfterConsecutiveFailures 验证语音连续失败达到阈值后进入暂停期：
// 暂停期内不再尝试语音（不重新上传），直接回退文字；暂停期结束后自动重试
func TestScreamVoicePausedAfterConsecutiveFailures(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{failSendVoice: true}
	addr := writeVoiceFile(t)
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 3, ScreamVoiceFile: addr}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	current := time.Date(2026, 8, 25, 10, 0, 0, 0, time.Local)
	nl.now = func() time.Time { return current }

	// 连续失败达到阈值：每次发送失败都会清除缓存，因此每次都重新上传
	for i := 0; i < maxVoiceFailures; i++ {
		nl.sendScream("chat-1")
	}
	fake.mu.Lock()
	if len(fake.uploads) != maxVoiceFailures {
		t.Fatalf("expected %d uploads before pause, got %d", maxVoiceFailures, len(fake.uploads))
	}
	sendsBeforePause := len(fake.sends)
	fake.mu.Unlock()

	// 暂停期内：直接回退文字，不再上传
	nl.sendScream("chat-1")
	fake.mu.Lock()
	if len(fake.uploads) != maxVoiceFailures {
		t.Fatalf("expected no upload while paused, got %d", len(fake.uploads))
	}
	if len(fake.sends) != sendsBeforePause+1 {
		t.Fatalf("expected text fallback while paused, got %d sends", len(fake.sends))
	}
	fake.mu.Unlock()

	// 暂停期结束且语音恢复可用：自动重试语音
	current = current.Add(voiceDisableDuration + time.Minute)
	fake.mu.Lock()
	fake.failSendVoice = false
	fake.mu.Unlock()

	nl.sendScream("chat-1")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.uploads) != maxVoiceFailures+1 {
		t.Fatalf("expected re-upload after pause expired, got %d uploads", len(fake.uploads))
	}
	if len(fake.sendVoices) != 1 {
		t.Fatalf("expected voice retry after pause expired, got %d voice sends", len(fake.sendVoices))
	}
}

// TestScreamVoiceReuploadsWhenFileChanged 验证同路径语音文件被替换后缓存失效并重新上传
func TestScreamVoiceReuploadsWhenFileChanged(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	addr := writeVoiceFile(t)
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 3, ScreamVoiceFile: addr}, fake, logger)
	nl.SetClient(fake)
	nl.captureChatID("group", "chat-1")

	nl.sendScream("chat-1")

	// 同路径替换为不同内容的文件：大小变化确保绕过文件系统时间戳精度问题
	if err := os.WriteFile(addr, []byte("#!AMR\n voice-data-v2-with-different-size"), 0o600); err != nil {
		t.Fatalf("rewrite voice file: %v", err)
	}
	nl.sendScream("chat-1")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.uploads) != 2 {
		t.Fatalf("expected re-upload after file change, got %d uploads", len(fake.uploads))
	}
	if len(fake.sendVoices) != 2 {
		t.Fatalf("expected 2 voice sends, got %d", len(fake.sendVoices))
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

func TestCaptureChatIDPersistsTargets(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	nl := New(&config.Config{CooldownMinutes: 1, TargetChatID: "configured"}, nil, logger)

	var mu sync.Mutex
	var persisted []string
	nl.SetTargetChatIDsPersister(func(value string) error {
		mu.Lock()
		defer mu.Unlock()
		persisted = append(persisted, value)
		return nil
	})

	nl.captureChatID("group", "chat-1")
	nl.captureChatID("group", "chat-1") // 重复发现不再写回
	nl.captureChatID("group", "chat-2")
	nl.captureChatID("single", "chat-3") // 非群聊不触发写回

	want := []string{"configured,chat-1", "configured,chat-1,chat-2"}
	mu.Lock()
	defer mu.Unlock()
	if len(persisted) != len(want) {
		t.Fatalf("persisted = %v, want %v", persisted, want)
	}
	for i := range want {
		if persisted[i] != want[i] {
			t.Fatalf("persisted[%d] = %q, want %q", i, persisted[i], want[i])
		}
	}
}

func TestFailureEvictionPersistsTargets(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{failSend: true}
	nl := New(&config.Config{CooldownMinutes: 1, MaxSendFailures: 1}, fake, logger)
	nl.SetClient(fake)

	var mu sync.Mutex
	var persisted []string
	nl.SetTargetChatIDsPersister(func(value string) error {
		mu.Lock()
		defer mu.Unlock()
		persisted = append(persisted, value)
		return nil
	})

	nl.captureChatID("group", "chat-1")
	nl.captureChatID("group", "chat-2")
	nl.sendScream("chat-1") // 连续失败达到上限，chat-1 被移出

	mu.Lock()
	defer mu.Unlock()
	want := []string{"chat-1", "chat-1,chat-2", "chat-2"}
	if len(persisted) != len(want) {
		t.Fatalf("persisted = %v, want %v", persisted, want)
	}
	for i := range want {
		if persisted[i] != want[i] {
			t.Fatalf("persisted[%d] = %q, want %q", i, persisted[i], want[i])
		}
	}
}

func TestPersistErrorDoesNotAffectCapture(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	nl := New(&config.Config{CooldownMinutes: 1}, nil, logger)
	nl.SetTargetChatIDsPersister(func(value string) error {
		return fmt.Errorf("disk full")
	})

	nl.captureChatID("group", "chat-1")

	if got := nl.targetChatIDs(); len(got) != 1 || got[0] != "chat-1" {
		t.Fatalf("targets = %v, want [chat-1]", got)
	}
}
