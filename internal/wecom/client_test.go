package wecom

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type testHandler struct {
	called atomic.Bool
	reqID  string
	body   *MsgCallbackBody
	event  *EventCallbackBody
}

func (h *testHandler) OnMessage(reqID string, body *MsgCallbackBody) {
	h.called.Store(true)
	h.reqID = reqID
	h.body = body
}

func (h *testHandler) OnEvent(body *EventCallbackBody) {
	h.event = body
}

func TestClientAuthAndReceiveMessage(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	var (
		authChecked atomic.Bool
		connected   = make(chan struct{})
		once        sync.Once
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		// 读取认证帧
		var authReq Frame
		if err := conn.ReadJSON(&authReq); err != nil {
			// 可能是连接被测试取消，直接返回
			return
		}
		if authReq.Cmd != "aibot_subscribe" {
			t.Errorf("unexpected cmd: %s", authReq.Cmd)
			return
		}
		var authBody AuthBody
		if err := json.Unmarshal(authReq.Body, &authBody); err != nil {
			t.Errorf("unmarshal auth body: %v", err)
			return
		}
		if authBody.BotID != "bot-id" || authBody.Secret != "secret" {
			t.Errorf("unexpected auth body: %+v", authBody)
			return
		}
		authChecked.Store(true)

		// 发送认证成功响应
		if err := conn.WriteJSON(Frame{Cmd: "aibot_subscribe", ErrCode: 0}); err != nil {
			t.Errorf("write auth resp: %v", err)
			return
		}
		once.Do(func() { close(connected) })

		// 发送一条消息回调
		msg := Frame{
			Cmd:     "aibot_msg_callback",
			Headers: Headers{ReqID: "req-123"},
			Body: mustMarshal(t, MsgCallbackBody{
				MsgID:    "msg-1",
				AIBotID:  "bot-id",
				ChatID:   "chat-1",
				ChatType: "group",
				From:     MsgFrom{UserID: "user-1"},
				MsgType:  "text",
				Text:     TextBody{Content: "牛来"},
				Mention:  []Mention{{UserID: "bot-id", Type: 1}},
			}),
		}
		if err := conn.WriteJSON(msg); err != nil {
			t.Errorf("write msg callback: %v", err)
			return
		}

		// 保持连接直到测试结束，避免客户端无限重连
		select {
		case <-ctx.Done():
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	handler := &testHandler{}
	client := NewClient("bot-id", "secret", handler, logger).SetURL(wsTestURL(t, server))

	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = client.Run(ctx)
	}()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		cancel()
		runWG.Wait()
		t.Fatal("client did not connect in time")
	}

	// 等待消息处理
	time.Sleep(300 * time.Millisecond)

	cancel()
	runWG.Wait()

	if !authChecked.Load() {
		t.Fatal("auth was not checked")
	}
	if !handler.called.Load() {
		t.Fatal("handler was not called")
	}
	if handler.reqID != "req-123" {
		t.Fatalf("unexpected reqID: %s", handler.reqID)
	}
	if handler.body == nil || handler.body.MsgID != "msg-1" {
		t.Fatalf("unexpected body: %+v", handler.body)
	}
}

func TestClientEventCallbackIsDelivered(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	var (
		connected = make(chan struct{})
		once      sync.Once
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		var authReq Frame
		if err := conn.ReadJSON(&authReq); err != nil {
			return
		}
		if err := conn.WriteJSON(Frame{Cmd: "aibot_subscribe", ErrCode: 0}); err != nil {
			t.Errorf("write auth resp: %v", err)
			return
		}
		once.Do(func() { close(connected) })

		event := Frame{
			Cmd:     "aibot_event_callback",
			Headers: Headers{ReqID: "req-event"},
			Body:    mustMarshal(t, EventCallbackBody{ChatID: "chat-event", ChatType: "group"}),
		}
		if err := conn.WriteJSON(event); err != nil {
			t.Errorf("write event callback: %v", err)
			return
		}

		select {
		case <-ctx.Done():
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	handler := &testHandler{}
	client := NewClient("bot-id", "secret", handler, logger).SetURL(wsTestURL(t, server))

	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = client.Run(ctx)
	}()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		cancel()
		runWG.Wait()
		t.Fatal("client did not connect in time")
	}

	time.Sleep(200 * time.Millisecond)
	cancel()
	runWG.Wait()

	if handler.event == nil {
		t.Fatal("event handler was not called")
	}
	if handler.event.ChatID != "chat-event" || handler.event.ChatType != "group" {
		t.Fatalf("unexpected event body: %+v", handler.event)
	}
}

func TestClientSendMarkdown(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	var (
		received  []Frame
		mu        sync.Mutex
		connected = make(chan struct{})
		once      sync.Once
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		// 认证响应
		var authReq Frame
		if err := conn.ReadJSON(&authReq); err != nil {
			return
		}
		if err := conn.WriteJSON(Frame{Cmd: "aibot_subscribe", ErrCode: 0}); err != nil {
			t.Errorf("write auth resp: %v", err)
			return
		}
		once.Do(func() { close(connected) })

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			var f Frame
			if err := conn.ReadJSON(&f); err != nil {
				return
			}
			mu.Lock()
			received = append(received, f)
			mu.Unlock()
			if f.Cmd == "aibot_send_msg" {
				_ = conn.WriteJSON(Frame{Cmd: f.Cmd, Headers: Headers{ReqID: f.Headers.ReqID}, ErrCode: 0})
			}
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	client := NewClient("bot-id", "secret", nil, logger).SetURL(wsTestURL(t, server))

	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = client.Run(ctx)
	}()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		cancel()
		runWG.Wait()
		t.Fatal("client did not connect in time")
	}

	if err := client.SendMarkdown(context.Background(), "chat-1", ChatTypeGroup, "妈妈"); err != nil {
		cancel()
		runWG.Wait()
		t.Fatalf("send markdown: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	cancel()
	runWG.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(received) == 0 {
		t.Fatal("no frame received")
	}
	last := received[len(received)-1]
	if last.Cmd != "aibot_send_msg" {
		t.Fatalf("unexpected cmd: %s", last.Cmd)
	}
	var body SendMsgBody
	if err := json.Unmarshal(last.Body, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body.ChatID != "chat-1" || body.MsgType != "markdown" || body.Markdown.Content != "妈妈" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

// TestClientSendVoiceServerError 验证服务端拒绝 aibot_send_msg（errcode 非 0）时
// 错误能返回给调用方，以便上层回退为文字消息
func TestClientSendVoiceServerError(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		var authReq Frame
		if err := conn.ReadJSON(&authReq); err != nil {
			return
		}
		if err := conn.WriteJSON(Frame{Cmd: "aibot_subscribe", ErrCode: 0}); err != nil {
			return
		}
		close(connected)

		for {
			var f Frame
			if err := conn.ReadJSON(&f); err != nil {
				return
			}
			if f.Cmd == "aibot_send_msg" {
				_ = conn.WriteJSON(Frame{
					Cmd:     f.Cmd,
					Headers: Headers{ReqID: f.Headers.ReqID},
					ErrCode: 60011,
					ErrMsg:  "no privilege to send voice",
				})
			}
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	client := NewClient("bot-id", "secret", nil, logger).SetURL(wsTestURL(t, server))

	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = client.Run(ctx)
	}()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		cancel()
		runWG.Wait()
		t.Fatal("client did not connect in time")
	}

	err := client.SendVoice(context.Background(), "chat-1", ChatTypeGroup, "media-1")
	cancel()
	runWG.Wait()

	if err == nil {
		t.Fatal("expected error when server rejects voice message")
	}
	if !strings.Contains(err.Error(), "60011") {
		t.Fatalf("expected errcode in error, got %v", err)
	}
}

func TestClientRespondImage(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	var (
		received  []Frame
		mu        sync.Mutex
		connected = make(chan struct{})
		once      sync.Once
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		var authReq Frame
		if err := conn.ReadJSON(&authReq); err != nil {
			return
		}
		if err := conn.WriteJSON(Frame{Cmd: "aibot_subscribe", ErrCode: 0}); err != nil {
			return
		}
		once.Do(func() { close(connected) })

		for {
			var f Frame
			if err := conn.ReadJSON(&f); err != nil {
				return
			}
			mu.Lock()
			received = append(received, f)
			mu.Unlock()
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	client := NewClient("bot-id", "secret", nil, logger).SetURL(wsTestURL(t, server))

	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = client.Run(ctx)
	}()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		cancel()
		runWG.Wait()
		t.Fatal("client did not connect in time")
	}

	if err := client.RespondImage("req-callback", "media-1"); err != nil {
		cancel()
		runWG.Wait()
		t.Fatalf("respond image: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	cancel()
	runWG.Wait()

	mu.Lock()
	defer mu.Unlock()
	var respond *Frame
	for i := range received {
		if received[i].Cmd == "aibot_respond_msg" {
			respond = &received[i]
		}
	}
	if respond == nil {
		t.Fatal("no respond frame received")
	}
	if respond.Headers.ReqID != "req-callback" {
		t.Fatalf("respond req_id = %q, want req-callback", respond.Headers.ReqID)
	}
	var body RespondMsgBody
	if err := json.Unmarshal(respond.Body, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body.MsgType != "image" || body.Image == nil || body.Image.MediaID != "media-1" {
		t.Fatalf("unexpected body: %+v", body)
	}
	if body.Markdown != nil {
		t.Fatalf("markdown should be omitted for image reply: %+v", body.Markdown)
	}
}

func TestClientUploadMedia(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	var (
		chunks    []UploadMediaChunkBody
		initGot   UploadMediaInitBody
		mu        sync.Mutex
		connected = make(chan struct{})
		once      sync.Once
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		var authReq Frame
		if err := conn.ReadJSON(&authReq); err != nil {
			return
		}
		if err := conn.WriteJSON(Frame{Cmd: "aibot_subscribe", ErrCode: 0}); err != nil {
			return
		}
		once.Do(func() { close(connected) })

		for {
			var f Frame
			if err := conn.ReadJSON(&f); err != nil {
				return
			}

			// 响应帧不带 cmd，模拟真实协议，仅靠 req_id 关联
			respond := func(body any) {
				resp := Frame{Headers: Headers{ReqID: f.Headers.ReqID}, Body: mustMarshal(t, body)}
				if err := conn.WriteJSON(resp); err != nil {
					t.Errorf("write response: %v", err)
				}
			}

			switch f.Cmd {
			case "aibot_upload_media_init":
				var body UploadMediaInitBody
				if err := json.Unmarshal(f.Body, &body); err != nil {
					t.Errorf("unmarshal init: %v", err)
					return
				}
				mu.Lock()
				initGot = body
				mu.Unlock()
				respond(UploadMediaInitResp{UploadID: "upload-1"})
			case "aibot_upload_media_chunk":
				var body UploadMediaChunkBody
				if err := json.Unmarshal(f.Body, &body); err != nil {
					t.Errorf("unmarshal chunk: %v", err)
					return
				}
				mu.Lock()
				chunks = append(chunks, body)
				mu.Unlock()
				respond(struct{}{})
			case "aibot_upload_media_finish":
				respond(UploadMediaFinishResp{MediaID: "media-final", Type: "image", CreatedAt: 1})
			}
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	client := NewClient("bot-id", "secret", nil, logger).SetURL(wsTestURL(t, server))

	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = client.Run(ctx)
	}()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		cancel()
		runWG.Wait()
		t.Fatal("client did not connect in time")
	}

	data := bytes.Repeat([]byte{0x5A}, 1000)
	mediaID, err := client.UploadMedia(ctx, "image", "cow.png", data)
	if err != nil {
		cancel()
		runWG.Wait()
		t.Fatalf("upload media: %v", err)
	}
	if mediaID != "media-final" {
		t.Fatalf("mediaID = %q, want media-final", mediaID)
	}

	cancel()
	runWG.Wait()

	mu.Lock()
	defer mu.Unlock()
	if initGot.Type != "image" || initGot.Filename != "cow.png" || initGot.TotalSize != 1000 || initGot.TotalChunks != 1 {
		t.Fatalf("unexpected init body: %+v", initGot)
	}
	if len(chunks) != 1 || chunks[0].UploadID != "upload-1" || chunks[0].ChunkIndex != 0 {
		t.Fatalf("unexpected chunks: %+v", chunks)
	}
	decoded, err := base64.StdEncoding.DecodeString(chunks[0].Base64Data)
	if err != nil {
		t.Fatalf("decode chunk: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Fatal("chunk data mismatch")
	}
}

func TestClientUploadMediaErrorResponse(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	connected := make(chan struct{})
	var once sync.Once

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var authReq Frame
		if err := conn.ReadJSON(&authReq); err != nil {
			return
		}
		if err := conn.WriteJSON(Frame{Cmd: "aibot_subscribe", ErrCode: 0}); err != nil {
			return
		}
		once.Do(func() { close(connected) })

		for {
			var f Frame
			if err := conn.ReadJSON(&f); err != nil {
				return
			}
			_ = conn.WriteJSON(Frame{
				Headers: Headers{ReqID: f.Headers.ReqID},
				ErrCode: 60011,
				ErrMsg:  "no permission",
			})
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	client := NewClient("bot-id", "secret", nil, logger).SetURL(wsTestURL(t, server))

	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = client.Run(ctx)
	}()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		cancel()
		runWG.Wait()
		t.Fatal("client did not connect in time")
	}

	_, err := client.UploadMedia(ctx, "image", "cow.png", bytes.Repeat([]byte{0x01}, 100))
	cancel()
	runWG.Wait()

	if err == nil {
		t.Fatal("expected error from errcode response")
	}
	if !strings.Contains(err.Error(), "60011") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestClientUploadMediaSizeLimits 验证客户端侧的素材大小守卫在发起网络请求前生效
func TestClientUploadMediaSizeLimits(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	client := NewClient("bot-id", "secret", nil, logger)

	if _, err := client.UploadMedia(context.Background(), "voice", "scream.amr", bytes.Repeat([]byte{0x01}, MaxVoiceMediaSize+1)); err == nil {
		t.Fatal("expected error for oversize voice media")
	}
	if _, err := client.UploadMedia(context.Background(), "image", "cow.png", bytes.Repeat([]byte{0x01}, MaxImageMediaSize+1)); err == nil {
		t.Fatal("expected error for oversize image media")
	}
}

func TestClientUploadMediaValidatesInput(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	client := NewClient("bot-id", "secret", nil, logger)

	if _, err := client.UploadMedia(context.Background(), "image", "tiny.png", []byte{0x01}); err == nil {
		t.Fatal("expected error for tiny media")
	}

	oversize := make([]byte, MaxImageMediaSize+1)
	if _, err := client.UploadMedia(context.Background(), "image", "big.png", oversize); err == nil {
		t.Fatal("expected error for oversize image")
	}
}

func TestClientUploadMediaRetriesFailedChunk(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	var (
		chunkAttempts int
		initCount     int
		mu            sync.Mutex
		connected     = make(chan struct{})
		once          sync.Once
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var authReq Frame
		if err := conn.ReadJSON(&authReq); err != nil {
			return
		}
		if err := conn.WriteJSON(Frame{Cmd: "aibot_subscribe", ErrCode: 0}); err != nil {
			return
		}
		once.Do(func() { close(connected) })

		for {
			var f Frame
			if err := conn.ReadJSON(&f); err != nil {
				return
			}

			respond := func(body any) {
				resp := Frame{Headers: Headers{ReqID: f.Headers.ReqID}, Body: mustMarshal(t, body)}
				if err := conn.WriteJSON(resp); err != nil {
					t.Errorf("write response: %v", err)
				}
			}

			switch f.Cmd {
			case "aibot_upload_media_init":
				mu.Lock()
				initCount++
				mu.Unlock()
				respond(UploadMediaInitResp{UploadID: "upload-1"})
			case "aibot_upload_media_chunk":
				mu.Lock()
				chunkAttempts++
				attempt := chunkAttempts
				mu.Unlock()
				if attempt == 1 {
					// 第一次分片上传失败，客户端应重试而不是放弃整个上传
					_ = conn.WriteJSON(Frame{
						Headers: Headers{ReqID: f.Headers.ReqID},
						ErrCode: 500,
						ErrMsg:  "server busy",
					})
					continue
				}
				respond(struct{}{})
			case "aibot_upload_media_finish":
				respond(UploadMediaFinishResp{MediaID: "media-final", Type: "image", CreatedAt: 1})
			}
		}
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	client := NewClient("bot-id", "secret", nil, logger).SetURL(wsTestURL(t, server))

	var runWG sync.WaitGroup
	runWG.Add(1)
	go func() {
		defer runWG.Done()
		_ = client.Run(ctx)
	}()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		cancel()
		runWG.Wait()
		t.Fatal("client did not connect in time")
	}

	mediaID, err := client.UploadMedia(ctx, "image", "cow.png", bytes.Repeat([]byte{0x5A}, 1000))
	cancel()
	runWG.Wait()

	if err != nil {
		t.Fatalf("upload media: %v", err)
	}
	if mediaID != "media-final" {
		t.Fatalf("mediaID = %q, want media-final", mediaID)
	}

	mu.Lock()
	defer mu.Unlock()
	if chunkAttempts != 2 {
		t.Fatalf("chunk attempts = %d, want 2 (1 failure + 1 retry)", chunkAttempts)
	}
	if initCount != 1 {
		t.Fatalf("init count = %d, want 1 (retry must not re-init)", initCount)
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestNextReconnectDelay(t *testing.T) {
	cases := []struct {
		current time.Duration
		want    time.Duration
	}{
		{0, time.Second},
		{time.Second, 2 * time.Second},
		{32 * time.Second, 60 * time.Second},
		{60 * time.Second, 60 * time.Second},
	}
	for _, c := range cases {
		got := nextReconnectDelay(c.current)
		if got != c.want {
			t.Errorf("nextReconnectDelay(%v) = %v, want %v", c.current, got, c.want)
		}
	}
}

func TestNewReqID(t *testing.T) {
	id1 := newReqID()
	id2 := newReqID()
	if id1 == id2 {
		t.Fatal("expected unique req ids")
	}
	if len(id1) != 16 {
		t.Fatalf("expected 16 hex chars, got %d", len(id1))
	}
}

func TestClientRunRespectsContext(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	client := NewClient("bot-id", "secret", nil, logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := client.Run(ctx)
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func wsTestURL(t *testing.T, server *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	u.Scheme = "ws"
	return u.String()
}
