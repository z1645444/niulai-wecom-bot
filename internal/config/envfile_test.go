package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEnvFile(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	return string(data)
}

func TestUpdateEnvFileReplacesExistingKey(t *testing.T) {
	content := "# credentials\nWECOM_BOT_ID=bot\n\nTARGET_CHAT_ID=old-chat\nLOG_LEVEL=debug\n"
	path := writeEnvFile(t, content, 0o600)

	if err := UpdateEnvFile(path, "TARGET_CHAT_ID", "old-chat,new-chat"); err != nil {
		t.Fatalf("UpdateEnvFile() error = %v", err)
	}

	want := "# credentials\nWECOM_BOT_ID=bot\n\nTARGET_CHAT_ID=old-chat,new-chat\nLOG_LEVEL=debug\n"
	if got := readFile(t, path); got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestUpdateEnvFilePreservesExportPrefixAndComment(t *testing.T) {
	path := writeEnvFile(t, "export TARGET_CHAT_ID=a,b  # 目标群\n", 0o600)

	if err := UpdateEnvFile(path, "TARGET_CHAT_ID", "a,b,c"); err != nil {
		t.Fatalf("UpdateEnvFile() error = %v", err)
	}

	want := "export TARGET_CHAT_ID=a,b,c # 目标群\n"
	if got := readFile(t, path); got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestUpdateEnvFileReplacesOnlyFirstOccurrence(t *testing.T) {
	path := writeEnvFile(t, "TARGET_CHAT_ID=a\nTARGET_CHAT_ID=b\n", 0o600)

	if err := UpdateEnvFile(path, "TARGET_CHAT_ID", "x"); err != nil {
		t.Fatalf("UpdateEnvFile() error = %v", err)
	}

	want := "TARGET_CHAT_ID=x\nTARGET_CHAT_ID=b\n"
	if got := readFile(t, path); got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestUpdateEnvFileAppendsMissingKey(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"trailing newline", "WECOM_BOT_ID=bot\n", "WECOM_BOT_ID=bot\nTARGET_CHAT_ID=chat-1\n"},
		{"no trailing newline", "WECOM_BOT_ID=bot", "WECOM_BOT_ID=bot\nTARGET_CHAT_ID=chat-1"},
		{"empty file", "", "TARGET_CHAT_ID=chat-1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEnvFile(t, tc.content, 0o600)
			if err := UpdateEnvFile(path, "TARGET_CHAT_ID", "chat-1"); err != nil {
				t.Fatalf("UpdateEnvFile() error = %v", err)
			}
			if got := readFile(t, path); got != tc.want {
				t.Fatalf("file = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUpdateEnvFileMissingFileIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")

	if err := UpdateEnvFile(path, "TARGET_CHAT_ID", "chat-1"); err != nil {
		t.Fatalf("UpdateEnvFile() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no file created, stat err = %v", err)
	}
}

func TestUpdateEnvFileRejectsNewlineValue(t *testing.T) {
	path := writeEnvFile(t, "TARGET_CHAT_ID=a\n", 0o600)

	if err := UpdateEnvFile(path, "TARGET_CHAT_ID", "a\nLOG_LEVEL=debug"); err == nil {
		t.Fatal("expected error for newline in value")
	}
	if got := readFile(t, path); got != "TARGET_CHAT_ID=a\n" {
		t.Fatalf("file must stay untouched, got %q", got)
	}
}

func TestUpdateEnvFilePreservesFilePermissions(t *testing.T) {
	path := writeEnvFile(t, "TARGET_CHAT_ID=a\n", 0o640)

	if err := UpdateEnvFile(path, "TARGET_CHAT_ID", "a,b"); err != nil {
		t.Fatalf("UpdateEnvFile() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("perm = %o, want %o", got, 0o640)
	}
}

func TestUpdateEnvFileKeepsCRLF(t *testing.T) {
	path := writeEnvFile(t, "WECOM_BOT_ID=bot\r\nTARGET_CHAT_ID=a\r\n", 0o600)

	if err := UpdateEnvFile(path, "TARGET_CHAT_ID", "a,b"); err != nil {
		t.Fatalf("UpdateEnvFile() error = %v", err)
	}

	want := "WECOM_BOT_ID=bot\r\nTARGET_CHAT_ID=a,b\r\n"
	if got := readFile(t, path); got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}

func TestUpdateEnvFileIgnoresCommentAndBlankLines(t *testing.T) {
	content := "# TARGET_CHAT_ID=commented\n\n  \nWECOM_BOT_ID=bot\n"
	path := writeEnvFile(t, content, 0o600)

	if err := UpdateEnvFile(path, "TARGET_CHAT_ID", "chat-1"); err != nil {
		t.Fatalf("UpdateEnvFile() error = %v", err)
	}

	want := "# TARGET_CHAT_ID=commented\n\n  \nWECOM_BOT_ID=bot\nTARGET_CHAT_ID=chat-1\n"
	if got := readFile(t, path); got != want {
		t.Fatalf("file = %q, want %q", got, want)
	}
}
