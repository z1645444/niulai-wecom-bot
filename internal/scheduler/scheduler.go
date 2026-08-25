package scheduler

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"niulai-wecom-bot/internal/config"
	"niulai-wecom-bot/internal/state"
)

// Scheduler 负责在工作时间内为每个目标群聊独立随机触发牛来事件
type Scheduler struct {
	cfg        *config.Config
	chats      func() []string
	machineFor func(chatID string) *state.Machine
	onTrigger  func(chatID string)
	checkEvery time.Duration

	// pendingToday 报告指定群聊当天是否尚未触发，用于每日保底触发；
	// 由业务方注入，未设置时保底逻辑不生效
	pendingToday func(chatID string) bool

	// 可注入的依赖，便于测试
	now      func() time.Time
	randIntn func(int) int

	// running 用原子操作防止多次启动
	running atomic.Bool

	stopMu   sync.Mutex
	stopCh   chan struct{}
	stopOnce sync.Once
}

// New 创建一个新的调度器。
// chats 返回当前目标群聊列表；machineFor 返回指定群聊的独立状态机；
// onTrigger 是单个群聊触发成功时的回调，由调用方负责把该群状态切换到 SCREAMING
func New(cfg *config.Config, chats func() []string, machineFor func(chatID string) *state.Machine, onTrigger func(chatID string)) *Scheduler {
	return &Scheduler{
		cfg:        cfg,
		chats:      chats,
		machineFor: machineFor,
		onTrigger:  onTrigger,
		checkEvery: 5 * time.Minute,
		stopCh:     make(chan struct{}),
		now:        time.Now,
		randIntn:   rand.Intn,
	}
}

// Start 启动调度循环，可多次调用，每次调用都会重置停止状态
func (s *Scheduler) Start(ctx context.Context) {
	if !s.running.CompareAndSwap(false, true) {
		return
	}
	defer s.running.Store(false)

	s.stopMu.Lock()
	s.stopCh = make(chan struct{})
	s.stopOnce = sync.Once{}
	s.stopMu.Unlock()

	ticker := time.NewTicker(s.checkEvery)
	defer ticker.Stop()

	// 启动时立即检查一次
	s.tick()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// Stop 停止调度循环，可安全重复调用；调用后仍可再次 Start
func (s *Scheduler) Stop() {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	s.stopOnce.Do(func() {
		if s.stopCh != nil {
			close(s.stopCh)
		}
	})
}

func (s *Scheduler) tick() {
	now := s.now()
	workTime := s.cfg.IsWorkTime(now)

	// 每个群聊的触发与冷却相互独立，逐个判定
	for _, chatID := range s.chats() {
		machine := s.machineFor(chatID)

		// 先检查冷却是否到期，即使不在工作时间也复位
		machine.ResetCooldown()

		if !workTime {
			continue
		}
		if machine.Current() != state.IDLE {
			continue
		}

		// 在工作时间内，按概率触发
		// 每 5 分钟检查一次，若工作 9 小时，则每个群聊约有 108 次检查
		// 设置每次检查触发概率为 5%，期望每个群聊每个工作日触发约 5 次
		// 如需更低频，可调整此概率
		if s.randIntn(100) < 5 || s.shouldForceTrigger(chatID, now) {
			s.onTrigger(chatID)
		}
	}
}

// shouldForceTrigger 实现“每个群聊每天至少触发一次”的保底：
// 进入工作结束前 FORCE_TRIGGER_WINDOW_MINUTES 分钟的窗口后，
// 若该群聊当天未触发，则不再依赖概率，直接触发
func (s *Scheduler) shouldForceTrigger(chatID string, now time.Time) bool {
	if s.pendingToday == nil || !s.pendingToday(chatID) {
		return false
	}
	window := time.Duration(s.cfg.ForceTriggerWindowMinutes) * time.Minute
	if window <= 0 {
		return false
	}
	// 使用 now 的时区复原当天工作结束时刻，与 IsWorkTime 的本地时钟语义保持一致
	end := time.Date(now.Year(), now.Month(), now.Day(),
		s.cfg.WorkEndTime.Hour(), s.cfg.WorkEndTime.Minute(), 0, 0, now.Location())
	return !now.Add(window).Before(end)
}

// SetPendingToday 注入“指定群聊当天是否尚未触发”的判定函数
func (s *Scheduler) SetPendingToday(fn func(chatID string) bool) {
	s.pendingToday = fn
}

// SetCheckInterval 用于测试时调整检查周期
func (s *Scheduler) SetCheckInterval(d time.Duration) {
	s.checkEvery = d
}
