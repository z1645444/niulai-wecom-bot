package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"niulai-wecom-bot/internal/bot"
	"niulai-wecom-bot/internal/config"
	"niulai-wecom-bot/internal/wecom"
)

func main() {
	// 本地开发时支持 .env 文件；生产环境直接读取环境变量
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logger, logFile := newLogger(cfg.LogLevel)
	if logFile != nil {
		defer logFile.Close()
	}
	logger.Info("config loaded",
		"bot_id", cfg.WeComBotID,
		"target_chat_id", cfg.TargetChatID,
	)

	// 牛来实现 wecom.Handler 接口
	nl := bot.New(cfg, nil, logger)

	client := wecom.NewClient(cfg.WeComBotID, cfg.WeComBotSecret, nl, logger)
	nl.SetClient(client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	nl.Start(ctx)

	// 启动 WebSocket 连接循环
	go func() {
		if err := client.Run(ctx); err != nil {
			logger.Error("wecom client exited", "err", err)
		}
		// WS 断开后停止整个应用，或根据需求决定是否退出
		cancel()
	}()

	// 等待中断信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigCh:
		logger.Info("shutdown signal received")
	case <-ctx.Done():
		logger.Info("context done")
	}

	nl.Stop()
	logger.Info("exited")
}

func newLogger(level string) (*slog.Logger, *os.File) {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}

	// 日志写入当前工作目录下的 log/，按进程启动时间戳命名；
	// 同时保留 stdout 输出，创建失败时退化为仅 stdout
	w := io.Writer(os.Stdout)
	f, err := openLogFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open log file, logging to stdout only: %v\n", err)
	} else {
		w = io.MultiWriter(os.Stdout, f)
	}

	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv})
	return slog.New(handler), f
}

// logRetention 日志文件保留时长，过期文件在启动时清理
const logRetention = 7 * 24 * time.Hour

func openLogFile() (*os.File, error) {
	dir := "log"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pruneLogs(dir)

	name := fmt.Sprintf("niulai-%s.log", time.Now().Format("20060102-150405"))
	return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// pruneLogs 删除超过保留期的日志文件，避免 log/ 目录无限增长；失败静默忽略
func pruneLogs(dir string) {
	cutoff := time.Now().Add(-logRetention)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "niulai-") || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
