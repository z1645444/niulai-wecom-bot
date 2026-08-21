package wecom

import (
	"context"
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

	if err := client.SendMarkdown("chat-1", ChatTypeGroup, "妈妈"); err != nil {
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
