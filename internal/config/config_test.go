package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadRequired(t *testing.T) {
	_ = os.Unsetenv("WECOM_BOT_ID")
	_ = os.Unsetenv("WECOM_BOT_SECRET")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing credentials")
	}
}

func TestLoadTargetChatID(t *testing.T) {
	t.Setenv("WECOM_BOT_ID", "test-bot")
	t.Setenv("WECOM_BOT_SECRET", "test-secret")
	t.Setenv("TARGET_CHAT_ID", "target-chat, target-chat-2,target-chat")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.TargetChatID != "target-chat,target-chat-2" {
		t.Fatalf("TargetChatID = %q, want %q", cfg.TargetChatID, "target-chat,target-chat-2")
	}
}

func TestParseTargetChatIDs(t *testing.T) {
	got := ParseTargetChatIDs(" chat-1,chat-2,,chat-1, chat-3 ")
	want := []string{"chat-1", "chat-2", "chat-3"}
	if len(got) != len(want) {
		t.Fatalf("ParseTargetChatIDs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ParseTargetChatIDs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIsWorkTime(t *testing.T) {
	cfg := &Config{
		WorkStartTime: mustParse("09:00"),
		WorkEndTime:   mustParse("18:00"),
		WorkDays:      map[int]struct{}{1: {}, 2: {}, 3: {}, 4: {}, 5: {}},
	}

	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"monday morning", time.Date(2026, 8, 17, 9, 30, 0, 0, time.Local), true},   // 周一
		{"monday evening", time.Date(2026, 8, 17, 18, 30, 0, 0, time.Local), false}, // 周一 下班后
		{"sunday noon", time.Date(2026, 8, 16, 12, 0, 0, 0, time.Local), false},     // 周日
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cfg.IsWorkTime(c.now)
			if got != c.want {
				t.Fatalf("IsWorkTime(%v) = %v, want %v", c.now, got, c.want)
			}
		})
	}
}

func TestParseInt(t *testing.T) {
	cases := []struct {
		value    string
		defaultV int
		want     int
		wantErr  bool
	}{
		{"", 10, 10, false},
		{"20", 10, 20, false},
		{"abc", 10, 0, true},
		{"-5", 10, 0, true},
		{"0", 10, 0, true},
	}
	for _, c := range cases {
		t.Run(c.value, func(t *testing.T) {
			got, err := parseInt(c.value, c.defaultV)
			if (err != nil) != c.wantErr {
				t.Fatalf("parseInt(%q) err = %v, wantErr %v", c.value, err, c.wantErr)
			}
			if got != c.want {
				t.Fatalf("parseInt(%q) = %d, want %d", c.value, got, c.want)
			}
		})
	}
}

func mustParse(s string) time.Time {
	t, _ := time.Parse("15:04", s)
	return t
}
