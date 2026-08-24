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

func TestLoadFinishReplyDefaults(t *testing.T) {
	t.Setenv("WECOM_BOT_ID", "test-bot")
	t.Setenv("WECOM_BOT_SECRET", "test-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.FinishReplyType != ReplyTypeText {
		t.Fatalf("FinishReplyType = %q, want %q", cfg.FinishReplyType, ReplyTypeText)
	}
	if cfg.FinishReplyText != DefaultFinishReplyText {
		t.Fatalf("FinishReplyText = %q, want default", cfg.FinishReplyText)
	}
	if cfg.FinishReplyImageURL != "" {
		t.Fatalf("FinishReplyImageURL = %q, want empty", cfg.FinishReplyImageURL)
	}
	if cfg.ForceTriggerWindowMinutes != 30 {
		t.Fatalf("ForceTriggerWindowMinutes = %d, want 30", cfg.ForceTriggerWindowMinutes)
	}
}

func TestLoadFinishReplyImage(t *testing.T) {
	t.Setenv("WECOM_BOT_ID", "test-bot")
	t.Setenv("WECOM_BOT_SECRET", "test-secret")
	t.Setenv("FINISH_REPLY_TYPE", "image")
	t.Setenv("FINISH_REPLY_IMAGE_URL", "https://example.com/cow.png")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.FinishReplyType != ReplyTypeImage {
		t.Fatalf("FinishReplyType = %q, want %q", cfg.FinishReplyType, ReplyTypeImage)
	}
	if cfg.FinishReplyImageURL != "https://example.com/cow.png" {
		t.Fatalf("FinishReplyImageURL = %q", cfg.FinishReplyImageURL)
	}
}

func TestLoadFinishReplyImageRequiresURL(t *testing.T) {
	t.Setenv("WECOM_BOT_ID", "test-bot")
	t.Setenv("WECOM_BOT_SECRET", "test-secret")
	t.Setenv("FINISH_REPLY_TYPE", "image")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when image reply has no URL")
	}
}

func TestLoadFinishReplyTypeInvalid(t *testing.T) {
	t.Setenv("WECOM_BOT_ID", "test-bot")
	t.Setenv("WECOM_BOT_SECRET", "test-secret")
	t.Setenv("FINISH_REPLY_TYPE", "video")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid FINISH_REPLY_TYPE")
	}
}

func TestLoadForceTriggerWindowInvalid(t *testing.T) {
	t.Setenv("WECOM_BOT_ID", "test-bot")
	t.Setenv("WECOM_BOT_SECRET", "test-secret")
	t.Setenv("FORCE_TRIGGER_WINDOW_MINUTES", "0")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for zero FORCE_TRIGGER_WINDOW_MINUTES")
	}
}

func TestLoadStopKeywordAndScreamContentDefaults(t *testing.T) {
	t.Setenv("WECOM_BOT_ID", "test-bot")
	t.Setenv("WECOM_BOT_SECRET", "test-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.StopKeyword != DefaultStopKeyword {
		t.Fatalf("StopKeyword = %q, want %q", cfg.StopKeyword, DefaultStopKeyword)
	}
	if cfg.ScreamContent != DefaultScreamContent {
		t.Fatalf("ScreamContent = %q, want %q", cfg.ScreamContent, DefaultScreamContent)
	}
}

func TestLoadStopKeywordAndScreamContentCustom(t *testing.T) {
	t.Setenv("WECOM_BOT_ID", "test-bot")
	t.Setenv("WECOM_BOT_SECRET", "test-secret")
	t.Setenv("STOP_KEYWORD", "牛哥")
	t.Setenv("SCREAM_CONTENT", "  干活了  ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.StopKeyword != "牛哥" {
		t.Fatalf("StopKeyword = %q, want %q", cfg.StopKeyword, "牛哥")
	}
	if cfg.ScreamContent != "干活了" {
		t.Fatalf("ScreamContent = %q, want %q", cfg.ScreamContent, "干活了")
	}
}

func TestLoadStopKeywordNormalized(t *testing.T) {
	t.Setenv("WECOM_BOT_ID", "test-bot")
	t.Setenv("WECOM_BOT_SECRET", "test-secret")
	t.Setenv("STOP_KEYWORD", " 牛​来 ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.StopKeyword != "牛来" {
		t.Fatalf("StopKeyword = %q, want %q", cfg.StopKeyword, "牛来")
	}
}

func TestLoadStopKeywordBlank(t *testing.T) {
	t.Setenv("WECOM_BOT_ID", "test-bot")
	t.Setenv("WECOM_BOT_SECRET", "test-secret")
	t.Setenv("STOP_KEYWORD", "   ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.StopKeyword != DefaultStopKeyword {
		t.Fatalf("StopKeyword = %q, want default %q", cfg.StopKeyword, DefaultStopKeyword)
	}
}

func mustParse(s string) time.Time {
	t, _ := time.Parse("15:04", s)
	return t
}
