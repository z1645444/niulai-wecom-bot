package state

import (
	"testing"
	"time"
)

func TestMachineLifecycle(t *testing.T) {
	m := NewMachine()

	if m.Current() != IDLE {
		t.Fatalf("initial state = %v, want IDLE", m.Current())
	}

	if !m.StartScreaming() {
		t.Fatal("StartScreaming should succeed from IDLE")
	}
	if m.Current() != SCREAMING {
		t.Fatalf("state = %v, want SCREAMING", m.Current())
	}

	// 从 SCREAMING 不能再直接触发
	if m.StartScreaming() {
		t.Fatal("StartScreaming should fail from SCREAMING")
	}

	// 停止并进入冷却
	if !m.StopScreaming(100 * time.Millisecond) {
		t.Fatal("StopScreaming should succeed from SCREAMING")
	}
	if m.Current() != COOLDOWN {
		t.Fatalf("state = %v, want COOLDOWN", m.Current())
	}

	// 冷却未到期不能复位
	if m.ResetCooldown() {
		t.Fatal("ResetCooldown should fail before cooldown expires")
	}

	time.Sleep(150 * time.Millisecond)

	if !m.ResetCooldown() {
		t.Fatal("ResetCooldown should succeed after cooldown expires")
	}
	if m.Current() != IDLE {
		t.Fatalf("state = %v, want IDLE", m.Current())
	}
}
