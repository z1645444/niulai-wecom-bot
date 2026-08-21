package state

import (
	"sync"
	"time"
)

// State 表示牛来的当前状态
type State int

const (
	// IDLE 空闲状态，等待工作时间触发
	IDLE State = iota
	// SCREAMING 正在持续发送“妈妈”
	SCREAMING
	// COOLDOWN 冷却中，暂停触发
	COOLDOWN
)

func (s State) String() string {
	switch s {
	case IDLE:
		return "IDLE"
	case SCREAMING:
		return "SCREAMING"
	case COOLDOWN:
		return "COOLDOWN"
	default:
		return "UNKNOWN"
	}
}

// Machine 是线程安全的状态机
type Machine struct {
	mu       sync.RWMutex
	state    State
	cooldown time.Time
}

// NewMachine 创建一个初始为 IDLE 的状态机
func NewMachine() *Machine {
	return &Machine{state: IDLE}
}

// Current 返回当前状态
func (m *Machine) Current() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// StartScreaming 从 IDLE 切换到 SCREAMING只有 IDLE 状态会成功
func (m *Machine) StartScreaming() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != IDLE {
		return false
	}
	m.state = SCREAMING
	return true
}

// StopScreaming 从 SCREAMING 切换到 COOLDOWN，并设置冷却截止时间
func (m *Machine) StopScreaming(cooldown time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != SCREAMING {
		return false
	}
	m.state = COOLDOWN
	m.cooldown = time.Now().Add(cooldown)
	return true
}

// ResetCooldown 检查冷却是否到期，若到期则从 COOLDOWN 回到 IDLE
func (m *Machine) ResetCooldown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != COOLDOWN {
		return false
	}
	if time.Now().Before(m.cooldown) {
		return false
	}
	m.state = IDLE
	m.cooldown = time.Time{}
	return true
}

// CooldownUntil 返回冷却截止时间，仅在 COOLDOWN 状态下有效
func (m *Machine) CooldownUntil() (time.Time, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.state != COOLDOWN {
		return time.Time{}, false
	}
	return m.cooldown, true
}
