package bot

import (
	"context"
	"sync"
	"testing"
	"time"

	"log/slog"
	"os"

	"niulai-wecom-bot/internal/config"
	"niulai-wecom-bot/internal/state"
	"niulai-wecom-bot/internal/wecom"
)

type fakeSender struct {
	mu          sync.Mutex
	sends       []sendCall
	responds    []respondCall
	failSend    bool
	failRespond bool
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
	if f.failRespond {
		return errTest()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responds = append(f.responds, respondCall{reqID: reqID, content: content})
	return nil
}

func errTest() error {
	return errTestType{}
}

type errTestType struct{}

func (errTestType) Error() string { return "test error" }

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
			name: "mention another user",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				AIBotID: "bot-1",
				Text:    wecom.TextBody{Content: "牛来"},
				Mention: []wecom.Mention{{UserID: "user-2", Type: 1}},
			},
			want: false,
		},
		{
			name: "no mention",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				AIBotID: "bot-1",
				Text:    wecom.TextBody{Content: "牛来"},
			},
			want: false,
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
			name: "fallback when no aibotid",
			body: &wecom.MsgCallbackBody{
				MsgType: "text",
				Text:    wecom.TextBody{Content: "牛来"},
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

func TestAutoCaptureChatID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	nl := New(&config.Config{CooldownMinutes: 1}, nil, logger)

	nl.OnMessage("", &wecom.MsgCallbackBody{
		ChatType: "group",
		ChatID:   "chat-1",
		MsgType:  "text",
		Text:     wecom.TextBody{Content: "hello"},
	})

	if got := nl.currentChatID(); got != "chat-1" {
		t.Fatalf("currentChatID = %q, want chat-1", got)
	}

	// 第二次不应覆盖
	nl.OnMessage("", &wecom.MsgCallbackBody{
		ChatType: "group",
		ChatID:   "chat-2",
		MsgType:  "text",
		Text:     wecom.TextBody{Content: "hello"},
	})
	if got := nl.currentChatID(); got != "chat-1" {
		t.Fatalf("currentChatID = %q, want chat-1", got)
	}
}

func TestStopCommandStartsCooldown(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	nl := New(&config.Config{CooldownMinutes: 1}, fake, logger)
	nl.SetClient(fake)

	// 先进入尖叫状态
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

	fake.mu.Lock()
	if len(fake.responds) != 1 || fake.responds[0].content != stopConfirmText {
		t.Fatalf("unexpected responds: %+v", fake.responds)
	}
	fake.mu.Unlock()
}

func TestScreamLoopStopsImmediately(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	fake := &fakeSender{}
	cfg := &config.Config{
		CooldownMinutes:    1,
		MinIntervalSeconds: 600, // 很长，确保 stop 信号能打断
		MaxIntervalSeconds: 600,
	}
	nl := New(cfg, fake, logger)
	nl.SetClient(fake)

	ctx, cancel := context.WithCancel(context.Background())
	nl.ctx, nl.cancel = ctx, cancel

	if !nl.machine.StartScreaming() {
		t.Fatal("failed to start screaming")
	}

	stopCh := make(chan struct{})
	session := &screamSession{
		chatID:   "chat-1",
		chatType: wecom.ChatTypeGroup,
		stopCh:   stopCh,
	}

	nl.wg.Add(1)
	go func() {
		defer nl.wg.Done()
		nl.screamLoop(session)
	}()

	// 给它一点时间开始循环
	time.Sleep(50 * time.Millisecond)

	// 模拟停止指令：切换状态并发出停止信号
	nl.machine.StopScreaming(time.Minute)
	close(stopCh)

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
	// 至少发送了一次“妈妈”，但不会持续发送
	if len(fake.sends) == 0 {
		t.Fatal("expected at least one scream message")
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

	stopCh := make(chan struct{})
	session := &screamSession{
		chatID:   nl.currentChatID(),
		chatType: wecom.ChatTypeGroup,
		stopCh:   stopCh,
	}

	nl.wg.Add(1)
	go func() {
		defer nl.wg.Done()
		nl.screamLoop(session)
	}()

	time.Sleep(150 * time.Millisecond)
	close(stopCh)
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
