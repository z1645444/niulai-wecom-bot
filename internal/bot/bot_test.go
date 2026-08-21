package bot

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"niulai-wecom-bot/internal/config"
	"niulai-wecom-bot/internal/state"
	"niulai-wecom-bot/internal/wecom"
)

type fakeSender struct {
	mu       sync.Mutex
	sends    []sendCall
	responds []respondCall
	failSend bool
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

func TestTargetChatIDPrefersConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := &config.Config{
		CooldownMinutes: 1,
		TargetChatID:    "target-chat",
	}
	nl := New(cfg, nil, logger)

	nl.OnMessage("", &wecom.MsgCallbackBody{
		ChatType: "group",
		ChatID:   "chat-1",
		MsgType:  "text",
		Text:     wecom.TextBody{Content: "hello"},
	})

	got := nl.targetChatIDs()
	if len(got) != 1 || got[0] != "target-chat" {
		t.Fatalf("targetChatIDs = %v, want [target-chat]", got)
	}
}

func TestStopCommandStartsCooldownWithoutReply(t *testing.T) {
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

	fake.mu.Lock()
	if len(fake.responds) != 0 {
		t.Fatalf("expected no respond, got %+v", fake.responds)
	}
	fake.mu.Unlock()
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

	nl.sendScreamToTargets(context.Background())
	if nl.getSendFailures("target-chat") != 1 {
		t.Fatalf("expected configured target failures = 1, got %d", nl.getSendFailures("target-chat"))
	}

	// 配置的目标不会被驱逐
	nl.chatsMu.RLock()
	_, ok := nl.chats["target-chat"]
	nl.chatsMu.RUnlock()
	if ok {
		t.Fatal("configured target should not be in auto-discovered chats")
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
