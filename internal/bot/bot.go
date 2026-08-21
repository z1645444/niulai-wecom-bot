package bot

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"niulai-wecom-bot/internal/config"
	"niulai-wecom-bot/internal/scheduler"
	"niulai-wecom-bot/internal/state"
	"niulai-wecom-bot/internal/wecom"
)

const (
	screamContent   = "妈妈"
	stopConfirmText = "好的，我安静了"
	triggerKeyword  = "牛来"
)

// Sender 是 bot 对外发送消息所需的最小接口，便于测试与解耦
type Sender interface {
	SendMarkdown(chatID string, chatType uint32, content string) error
	RespondMarkdown(reqID, content string) error
}

// NiuLai 是虚拟员工“牛来”的业务编排器
type NiuLai struct {
	cfg       *config.Config
	machine   *state.Machine
	client    Sender
	scheduler *scheduler.Scheduler
	logger    *slog.Logger

	// activeChat 是运行时自动发现的活跃会话
	// 当 TARGET_CHAT_ID 为空时，从首次收到的群消息回调中获取
	activeChatMu sync.RWMutex
	activeChatID string

	// screamStop 用于唤醒 screamLoop 以便立即停止发送
	screamMu   sync.Mutex
	screamStop chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// screamSession 表示一次 SCREAMING 会话的上下文
// 由 onTrigger 创建并传递给 screamLoop，确保生命周期清晰
type screamSession struct {
	chatID   string
	chatType uint32
	stopCh   chan struct{}
}

// New 创建一个新的牛来实例
func New(cfg *config.Config, client Sender, logger *slog.Logger) *NiuLai {
	machine := state.NewMachine()
	nl := &NiuLai{
		cfg:     cfg,
		machine: machine,
		client:  client,
		logger:  logger,
	}
	nl.scheduler = scheduler.New(cfg, machine, nl.onTrigger)
	return nl
}

// Start 启动牛来的调度循环
func (nl *NiuLai) Start(ctx context.Context) {
	nl.ctx, nl.cancel = context.WithCancel(ctx)

	nl.wg.Add(1)
	go func() {
		defer nl.wg.Done()
		nl.scheduler.Start(nl.ctx)
	}()

	nl.logger.Info("niulai started",
		"work_start", nl.cfg.WorkStartTime.Format("15:04"),
		"work_end", nl.cfg.WorkEndTime.Format("15:04"),
		"min_interval", nl.cfg.MinIntervalSeconds,
		"max_interval", nl.cfg.MaxIntervalSeconds,
		"cooldown_minutes", nl.cfg.CooldownMinutes,
	)
}

// Stop 停止牛来，等待 goroutine 退出
func (nl *NiuLai) Stop() {
	if nl.cancel != nil {
		nl.cancel()
	}
	nl.scheduler.Stop()
	nl.signalScreamStop()
	nl.wg.Wait()
	nl.logger.Info("niulai stopped")
}

// SetClient 注入 WebSocket 客户端，解决初始化时的循环依赖
func (nl *NiuLai) SetClient(client Sender) {
	nl.client = client
}

// OnMessage 实现 wecom.Handler，处理收到的用户消息
func (nl *NiuLai) OnMessage(reqID string, body *wecom.MsgCallbackBody) {
	if body == nil {
		return
	}

	// 仅在文本消息时记录内容，避免非文本消息日志为空
	if body.MsgType == "text" {
		nl.logger.Debug("message received",
			"msgid", body.MsgID,
			"chatid", body.ChatID,
			"chattype", body.ChatType,
			"from", body.From.UserID,
			"msgtype", body.MsgType,
			"content", body.Text.Content,
		)
	} else {
		nl.logger.Debug("message received",
			"msgid", body.MsgID,
			"chatid", body.ChatID,
			"chattype", body.ChatType,
			"from", body.From.UserID,
			"msgtype", body.MsgType,
		)
	}

	// 自动发现目标群：当没有配置 TARGET_CHAT_ID 时，把首次收到的群消息 chatid 记下来
	nl.maybeCaptureChatID(body)

	// 去重、幂等可由调用方自行维护 msgid 集合
	// 这里只关注停止指令
	if !nl.isStopCommand(body) {
		return
	}

	cooldown := time.Duration(nl.cfg.CooldownMinutes) * time.Minute
	if !nl.machine.StopScreaming(cooldown) {
		nl.logger.Info("stop command ignored: not screaming", "state", nl.machine.Current())
		return
	}

	nl.signalScreamStop()

	nl.logger.Info("stop command accepted", "from", body.From.UserID, "chatid", body.ChatID)

	// 被动回复确认
	if reqID != "" && nl.client != nil {
		if err := nl.client.RespondMarkdown(reqID, stopConfirmText); err != nil {
			nl.logger.Warn("failed to send stop confirmation", "err", err)
		}
	}
}

func (nl *NiuLai) maybeCaptureChatID(body *wecom.MsgCallbackBody) {
	if body.ChatType != "group" || body.ChatID == "" {
		return
	}

	nl.activeChatMu.Lock()
	defer nl.activeChatMu.Unlock()

	if nl.activeChatID != "" {
		return
	}

	nl.activeChatID = body.ChatID
	nl.logger.Info("auto-captured target group chatid", "chatid", nl.activeChatID)
}

func (nl *NiuLai) currentChatID() string {
	nl.activeChatMu.RLock()
	defer nl.activeChatMu.RUnlock()

	if nl.activeChatID != "" {
		return nl.activeChatID
	}
	return nl.cfg.TargetChatID
}

func (nl *NiuLai) isStopCommand(body *wecom.MsgCallbackBody) bool {
	if body.MsgType != "text" {
		return false
	}
	content := strings.TrimSpace(body.Text.Content)
	if content == "" {
		return false
	}
	if !strings.Contains(content, triggerKeyword) {
		return false
	}

	// 要求消息中 @ 了当前机器人
	// 企业微信智能机器人回调里 mention 字段会包含被 @ 的用户/机器人 ID
	// 如果回调携带了 aibotid，则精确匹配；否则只要存在任何 @ 即认为命中
	if len(body.Mention) == 0 {
		return false
	}
	for _, m := range body.Mention {
		if body.AIBotID != "" {
			if m.UserID == body.AIBotID {
				return true
			}
			continue
		}
		// 无机器人 ID 时退化到原来的启发式判断
		if m.Type == 1 || m.UserID != "" {
			return true
		}
	}
	return false
}

func (nl *NiuLai) onTrigger() {
	if !nl.machine.StartScreaming() {
		return
	}

	nl.logger.Info("event triggered, start screaming")

	chatID := nl.currentChatID()
	chatType := wecom.ChatTypeGroup
	if chatID == "" {
		nl.logger.Warn("no TARGET_CHAT_ID configured, screaming will not send messages")
	}

	session := nl.newScreamSession(chatID, chatType)

	nl.wg.Add(1)
	go func() {
		defer nl.wg.Done()
		nl.screamLoop(session)
	}()
}

func (nl *NiuLai) newScreamSession(chatID string, chatType uint32) *screamSession {
	nl.screamMu.Lock()
	defer nl.screamMu.Unlock()

	// 关闭旧的停止通道（如果存在），确保上一个 screamLoop 立即退出
	if nl.screamStop != nil {
		close(nl.screamStop)
	}
	nl.screamStop = make(chan struct{})

	return &screamSession{
		chatID:   chatID,
		chatType: chatType,
		stopCh:   nl.screamStop,
	}
}

func (nl *NiuLai) screamLoop(session *screamSession) {
	for {
		select {
		case <-nl.ctx.Done():
			return
		case <-session.stopCh:
			return
		default:
		}

		if nl.machine.Current() != state.SCREAMING {
			return
		}

		if session.chatID != "" {
			if err := nl.client.SendMarkdown(session.chatID, session.chatType, screamContent); err != nil {
				nl.logger.Warn("failed to send scream message", "err", err)
			} else {
				nl.logger.Info("scream sent", "content", screamContent)
			}
		}

		interval := nl.cfg.RandomInterval()
		nl.logger.Debug("next scream interval", "interval", interval)

		select {
		case <-nl.ctx.Done():
			return
		case <-session.stopCh:
			return
		case <-time.After(interval):
			// 继续下一轮
		}
	}
}

func (nl *NiuLai) signalScreamStop() {
	nl.screamMu.Lock()
	defer nl.screamMu.Unlock()
	if nl.screamStop != nil {
		close(nl.screamStop)
		nl.screamStop = nil
	}
}
