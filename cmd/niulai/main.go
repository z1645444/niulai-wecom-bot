package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

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

	logger := newLogger(cfg.LogLevel)
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

func newLogger(level string) *slog.Logger {
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
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	return slog.New(handler)
}
