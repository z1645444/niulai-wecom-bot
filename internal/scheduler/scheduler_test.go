package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"niulai-wecom-bot/internal/config"
	"niulai-wecom-bot/internal/state"
)

// newSingleChatScheduler 构造只面向单个群聊（chat-1）的调度器，简化单会话场景的测试
func newSingleChatScheduler(cfg *config.Config, machine *state.Machine, onTrigger func(chatID string)) *Scheduler {
	return New(cfg,
		func() []string { return []string{"chat-1"} },
		func(string) *state.Machine { return machine },
		onTrigger,
	)
}

func TestSchedulerDoesNotTriggerOutsideWorkTime(t *testing.T) {
	var triggered atomic.Bool
	cfg := &config.Config{
		WorkStartTime: mustParseTime("09:00"),
		WorkEndTime:   mustParseTime("18:00"),
		WorkDays:      map[int]struct{}{1: {}}, // 仅周一
	}

	s := newSingleChatScheduler(cfg, state.NewMachine(), func(string) {
		triggered.Store(true)
	})
	s.now = func() time.Time {
		// 周日 12:00
		return time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	}
	s.randIntn = func(n int) int { return 0 }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.SetCheckInterval(50 * time.Millisecond)
	go s.Start(ctx)

	time.Sleep(120 * time.Millisecond)

	if triggered.Load() {
		t.Fatal("should not trigger outside work time")
	}
}

func TestSchedulerTriggersDuringWorkTime(t *testing.T) {
	var triggered atomic.Bool
	cfg := &config.Config{
		WorkStartTime: mustParseTime("09:00"),
		WorkEndTime:   mustParseTime("18:00"),
		WorkDays:      map[int]struct{}{1: {}},
	}

	s := newSingleChatScheduler(cfg, state.NewMachine(), func(string) {
		triggered.Store(true)
	})
	// 周一 10:00
	s.now = func() time.Time {
		return time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	}
	s.randIntn = func(n int) int { return 0 } // 确保命中 5% 概率

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.SetCheckInterval(50 * time.Millisecond)
	go s.Start(ctx)

	time.Sleep(120 * time.Millisecond)

	if !triggered.Load() {
		t.Fatal("expected trigger during work time")
	}
}

func TestSchedulerTriggersChatsIndependently(t *testing.T) {
	var mu sync.Mutex
	var triggered []string
	cfg := &config.Config{
		WorkStartTime: mustParseTime("09:00"),
		WorkEndTime:   mustParseTime("18:00"),
		WorkDays:      map[int]struct{}{1: {}},
	}

	// chat-1 已在 SCREAMING，chat-2 处于 IDLE：只有 chat-2 应被触发
	screaming := state.NewMachine()
	if !screaming.StartScreaming() {
		t.Fatal("failed to start screaming")
	}
	machines := map[string]*state.Machine{
		"chat-1": screaming,
		"chat-2": state.NewMachine(),
	}

	s := New(cfg,
		func() []string { return []string{"chat-1", "chat-2"} },
		func(chatID string) *state.Machine { return machines[chatID] },
		func(chatID string) {
			mu.Lock()
			triggered = append(triggered, chatID)
			mu.Unlock()
		},
	)
	s.now = func() time.Time {
		return time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	}
	s.randIntn = func(n int) int { return 0 }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.SetCheckInterval(50 * time.Millisecond)
	go s.Start(ctx)

	time.Sleep(120 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for _, chatID := range triggered {
		if chatID != "chat-2" {
			t.Fatalf("unexpected trigger for %q, want only chat-2", chatID)
		}
	}
	if len(triggered) == 0 {
		t.Fatal("expected chat-2 to trigger")
	}
}

func TestSchedulerStopCanBeCalledTwice(t *testing.T) {
	cfg := &config.Config{
		WorkStartTime: mustParseTime("09:00"),
		WorkEndTime:   mustParseTime("18:00"),
		WorkDays:      map[int]struct{}{1: {}},
	}
	s := newSingleChatScheduler(cfg, state.NewMachine(), func(string) {})

	ctx, cancel := context.WithCancel(context.Background())
	go s.Start(ctx)

	time.Sleep(50 * time.Millisecond)
	s.Stop()
	s.Stop() // 不应 panic

	cancel()
}

func TestSchedulerCanRestartAfterStop(t *testing.T) {
	var triggered atomic.Bool
	cfg := &config.Config{
		WorkStartTime: mustParseTime("09:00"),
		WorkEndTime:   mustParseTime("18:00"),
		WorkDays:      map[int]struct{}{1: {}},
	}

	machine := state.NewMachine()
	s := New(cfg,
		func() []string { return []string{"chat-1"} },
		func(string) *state.Machine { return machine },
		func(string) {
			triggered.Store(true)
		},
	)
	s.now = func() time.Time {
		return time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	}
	s.randIntn = func(n int) int { return 0 }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.SetCheckInterval(50 * time.Millisecond)
	go s.Start(ctx)
	time.Sleep(120 * time.Millisecond)
	s.Stop()

	// 等待第一次 Start 完全退出
	for i := 0; i < 50 && s.running.Load(); i++ {
		time.Sleep(10 * time.Millisecond)
	}

	if !triggered.Load() {
		t.Fatal("expected trigger during first run")
	}

	// 重置状态并重新触发
	triggered.Store(false)
	machine = state.NewMachine()

	go s.Start(ctx)
	time.Sleep(120 * time.Millisecond)
	s.Stop()

	if !triggered.Load() {
		t.Fatal("expected trigger after restart")
	}
}

func TestSchedulerForceTriggerNearWorkEnd(t *testing.T) {
	var triggered atomic.Bool
	cfg := &config.Config{
		WorkStartTime:             mustParseTime("09:00"),
		WorkEndTime:               mustParseTime("18:00"),
		WorkDays:                  map[int]struct{}{1: {}},
		ForceTriggerWindowMinutes: 30,
	}

	s := newSingleChatScheduler(cfg, state.NewMachine(), func(string) {
		triggered.Store(true)
	})
	s.SetPendingToday(func(string) bool { return true })
	// 周一 17:45，距工作结束 15 分钟，已进入保底窗口
	s.now = func() time.Time {
		return time.Date(2026, 8, 17, 17, 45, 0, 0, time.UTC)
	}
	s.randIntn = func(n int) int { return 99 } // 确保概率触发不命中

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.SetCheckInterval(50 * time.Millisecond)
	go s.Start(ctx)

	time.Sleep(120 * time.Millisecond)

	if !triggered.Load() {
		t.Fatal("expected force trigger inside the guarantee window")
	}
}

func TestSchedulerForceTriggerOnlyPendingChats(t *testing.T) {
	var mu sync.Mutex
	var triggered []string
	cfg := &config.Config{
		WorkStartTime:             mustParseTime("09:00"),
		WorkEndTime:               mustParseTime("18:00"),
		WorkDays:                  map[int]struct{}{1: {}},
		ForceTriggerWindowMinutes: 30,
	}

	machines := map[string]*state.Machine{
		"chat-1": state.NewMachine(),
		"chat-2": state.NewMachine(),
	}
	s := New(cfg,
		func() []string { return []string{"chat-1", "chat-2"} },
		func(chatID string) *state.Machine { return machines[chatID] },
		func(chatID string) {
			mu.Lock()
			triggered = append(triggered, chatID)
			mu.Unlock()
		},
	)
	// chat-1 今日已触发，只有 chat-2 处于待触发
	s.SetPendingToday(func(chatID string) bool { return chatID == "chat-2" })
	// 周一 17:45，已进入保底窗口
	s.now = func() time.Time {
		return time.Date(2026, 8, 17, 17, 45, 0, 0, time.UTC)
	}
	s.randIntn = func(n int) int { return 99 } // 确保概率触发不命中

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.SetCheckInterval(50 * time.Millisecond)
	go s.Start(ctx)

	time.Sleep(120 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for _, chatID := range triggered {
		if chatID != "chat-2" {
			t.Fatalf("unexpected force trigger for %q, want only chat-2", chatID)
		}
	}
	if len(triggered) == 0 {
		t.Fatal("expected force trigger for pending chat-2")
	}
}

func TestSchedulerNoForceTriggerWhenNothingPending(t *testing.T) {
	var triggered atomic.Bool
	cfg := &config.Config{
		WorkStartTime:             mustParseTime("09:00"),
		WorkEndTime:               mustParseTime("18:00"),
		WorkDays:                  map[int]struct{}{1: {}},
		ForceTriggerWindowMinutes: 30,
	}

	s := newSingleChatScheduler(cfg, state.NewMachine(), func(string) {
		triggered.Store(true)
	})
	s.SetPendingToday(func(string) bool { return false }) // 该群聊今日已触发
	s.now = func() time.Time {
		return time.Date(2026, 8, 17, 17, 45, 0, 0, time.UTC)
	}
	s.randIntn = func(n int) int { return 99 }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.SetCheckInterval(50 * time.Millisecond)
	go s.Start(ctx)

	time.Sleep(120 * time.Millisecond)

	if triggered.Load() {
		t.Fatal("should not force trigger when the chat was triggered today")
	}
}

func TestSchedulerNoForceTriggerOutsideWindow(t *testing.T) {
	var triggered atomic.Bool
	cfg := &config.Config{
		WorkStartTime:             mustParseTime("09:00"),
		WorkEndTime:               mustParseTime("18:00"),
		WorkDays:                  map[int]struct{}{1: {}},
		ForceTriggerWindowMinutes: 30,
	}

	s := newSingleChatScheduler(cfg, state.NewMachine(), func(string) {
		triggered.Store(true)
	})
	s.SetPendingToday(func(string) bool { return true })
	// 周一 10:00，远早于保底窗口
	s.now = func() time.Time {
		return time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	}
	s.randIntn = func(n int) int { return 99 }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.SetCheckInterval(50 * time.Millisecond)
	go s.Start(ctx)

	time.Sleep(120 * time.Millisecond)

	if triggered.Load() {
		t.Fatal("should not force trigger before the guarantee window")
	}
}

func TestSchedulerTriggerOnStartIgnoresWorkTime(t *testing.T) {
	// 无工作日：常规 tick 永不触发，若仍收到触发事件则只能来自启动触发
	triggered := make(chan string, 2)
	cfg := &config.Config{
		WorkStartTime:  mustParseTime("09:00"),
		WorkEndTime:    mustParseTime("18:00"),
		WorkDays:       map[int]struct{}{},
		TriggerOnStart: true,
	}

	machines := map[string]*state.Machine{
		"chat-1": state.NewMachine(),
		"chat-2": state.NewMachine(),
	}
	s := New(cfg,
		func() []string { return []string{"chat-1", "chat-2"} },
		func(chatID string) *state.Machine { return machines[chatID] },
		func(chatID string) { triggered <- chatID },
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Start(ctx)

	// 启动触发先于首次 tick 同步执行，事件到达即返回；1s 超时仅作失败兜底
	var got []string
	for len(got) < 2 {
		select {
		case chatID := <-triggered:
			got = append(got, chatID)
		case <-time.After(time.Second):
			t.Fatalf("expected both chats triggered on start, got %v", got)
		}
	}
}

func TestSchedulerTriggerOnStartSkipsScreamingChats(t *testing.T) {
	var triggered []string
	cfg := &config.Config{TriggerOnStart: true}

	// chat-1 已在 SCREAMING，chat-2 处于 IDLE：启动触发只应作用于 chat-2
	screaming := state.NewMachine()
	if !screaming.StartScreaming() {
		t.Fatal("failed to start screaming")
	}
	machines := map[string]*state.Machine{
		"chat-1": screaming,
		"chat-2": state.NewMachine(),
	}

	s := New(cfg,
		func() []string { return []string{"chat-1", "chat-2"} },
		func(chatID string) *state.Machine { return machines[chatID] },
		func(chatID string) { triggered = append(triggered, chatID) },
	)

	s.triggerAll()

	if len(triggered) != 1 || triggered[0] != "chat-2" {
		t.Fatalf("expected only chat-2 triggered on start, got %v", triggered)
	}
}

func mustParseTime(s string) time.Time {
	t, _ := time.Parse("15:04", s)
	return t
}
