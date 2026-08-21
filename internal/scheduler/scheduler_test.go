package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"niulai-wecom-bot/internal/config"
	"niulai-wecom-bot/internal/state"
)

func TestSchedulerDoesNotTriggerOutsideWorkTime(t *testing.T) {
	var triggered atomic.Bool
	cfg := &config.Config{
		WorkStartTime: mustParseTime("09:00"),
		WorkEndTime:   mustParseTime("18:00"),
		WorkDays:      map[int]struct{}{1: {}}, // 仅周一
	}
	machine := state.NewMachine()

	s := New(cfg, machine, func() {
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
	machine := state.NewMachine()

	s := New(cfg, machine, func() {
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

func TestSchedulerStopCanBeCalledTwice(t *testing.T) {
	cfg := &config.Config{
		WorkStartTime: mustParseTime("09:00"),
		WorkEndTime:   mustParseTime("18:00"),
		WorkDays:      map[int]struct{}{1: {}},
	}
	s := New(cfg, state.NewMachine(), func() {})

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

	s := New(cfg, machine, func() {
		triggered.Store(true)
	})
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
	s.machine = machine

	go s.Start(ctx)
	time.Sleep(120 * time.Millisecond)
	s.Stop()

	if !triggered.Load() {
		t.Fatal("expected trigger after restart")
	}
}

func mustParseTime(s string) time.Time {
	t, _ := time.Parse("15:04", s)
	return t
}
