package bot

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"niulai-wecom-bot/internal/config"
	"niulai-wecom-bot/internal/scheduler"
	"niulai-wecom-bot/internal/state"
	"niulai-wecom-bot/internal/wecom"
)

const (
	screamContent  = "妈妈"
	triggerKeyword = "牛来"
	zeroWidthSpace = '\u200B'
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

	// chats 是运行时自动发现的活跃群聊集合
	// 当没有配置 TARGET_CHAT_ID 时，从收到的群消息/事件回调中收集
	chatsMu sync.RWMutex
	chats   map[string]struct{}

	// failures 记录每个 chatid 的连续发送失败次数，与是否自动发现无关
	failuresMu sync.Mutex
	failures   map[string]int

	// screamCtx/screamCancel 控制当前 SCREAMING 会话的生命周期
	screamMu     sync.Mutex
	screamCtx    context.Context
	screamCancel context.CancelFunc

	// sleep 控制 screamLoop 的等待行为，便于测试注入
	sleep func(context.Context, time.Duration) bool

	running atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New 创建一个新的牛来实例
func New(cfg *config.Config, client Sender, logger *slog.Logger) *NiuLai {
	machine := state.NewMachine()
	nl := &NiuLai{
		cfg:      cfg,
		machine:  machine,
		client:   client,
		logger:   logger,
		chats:    make(map[string]struct{}),
		failures: make(map[string]int),
		sleep:    ctxSleep,
	}
	// 默认上下文，避免测试绕过 Start 时访问 nil ctx
	nl.ctx = context.Background()
	nl.scheduler = scheduler.New(cfg, machine, nl.onTrigger)
	return nl
}

// ctxSleep 是 screamLoop 默认的等待实现，可被测试替换
func ctxSleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// Start 启动牛来的调度循环；已启动时直接返回
func (nl *NiuLai) Start(ctx context.Context) {
	if !nl.running.CompareAndSwap(false, true) {
		nl.logger.Warn("niulai already started")
		return
	}

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
		"max_send_failures", nl.cfg.MaxSendFailures,
	)
}

// Stop 停止牛来，等待 goroutine 退出
func (nl *NiuLai) Stop() {
	if !nl.running.CompareAndSwap(true, false) {
		return
	}

	if nl.cancel != nil {
		nl.cancel()
	}
	nl.scheduler.Stop()
	nl.cancelScream()
	nl.wg.Wait()
	nl.logger.Info("niulai stopped")
}

// SetClient 注入 WebSocket 客户端，解决初始化时的循环依赖
func (nl *NiuLai) SetClient(client Sender) {
	nl.client = client
}

// OnMessage 实现 wecom.Handler，处理收到的用户消息。
// 文本中包含“牛来”即触发停止，不依赖企业微信回调中的 @ 字段。
func (nl *NiuLai) OnMessage(reqID string, body *wecom.MsgCallbackBody) {
	if body == nil {
		return
	}

	logAttrs := []any{
		"msgid", body.MsgID,
		"chatid", body.ChatID,
		"chattype", body.ChatType,
		"from", body.From.UserID,
		"msgtype", body.MsgType,
	}
	// 仅在文本消息时记录内容，避免非文本消息日志为空
	if body.MsgType == "text" {
		logAttrs = append(logAttrs, "content", body.Text.Content)
	}
	nl.logger.Debug("message received", logAttrs...)

	// 自动发现目标群：从任何群消息或群事件回调中收集 chatid
	nl.captureChatID(body.ChatType, body.ChatID)

	// 去重、幂等可由调用方自行维护 msgid 集合。
	// 这里只关注停止指令。
	if !nl.isStopCommand(body) {
		return
	}

	cooldown := time.Duration(nl.cfg.CooldownMinutes) * time.Minute
	if !nl.machine.StopScreaming(cooldown) {
		nl.logger.Info("stop command ignored: not screaming", "state", nl.machine.Current())
		return
	}

	nl.cancelScream()

	nl.logger.Info("stop command accepted", "from", body.From.UserID, "chatid", body.ChatID)
}

// OnEvent 实现 wecom.Handler，从事件回调中发现群聊
func (nl *NiuLai) OnEvent(body *wecom.EventCallbackBody) {
	if body == nil {
		return
	}
	nl.captureChatID(body.ChatType, body.ChatID)
}

func (nl *NiuLai) captureChatID(chatType, chatID string) {
	if chatType != "group" || chatID == "" {
		return
	}

	nl.chatsMu.Lock()
	defer nl.chatsMu.Unlock()

	if _, ok := nl.chats[chatID]; ok {
		return
	}

	nl.chats[chatID] = struct{}{}
	nl.logger.Info("auto-captured target group chatid", "chatid", chatID)
}

// targetChatIDs 返回当前需要发送消息的群聊 ID 列表
// 如果配置了 TARGET_CHAT_ID，则优先使用配置值；否则使用运行时收集的群聊列表
func (nl *NiuLai) targetChatIDs() []string {
	if nl.cfg.TargetChatID != "" {
		return []string{nl.cfg.TargetChatID}
	}

	nl.chatsMu.RLock()
	defer nl.chatsMu.RUnlock()

	if len(nl.chats) == 0 {
		return nil
	}

	ids := make([]string, 0, len(nl.chats))
	for id := range nl.chats {
		ids = append(ids, id)
	}
	return ids
}

func (nl *NiuLai) isStopCommand(body *wecom.MsgCallbackBody) bool {
	if body.MsgType != "text" {
		return false
	}
	// 去除所有 Unicode 空白字符（包括普通空格、Tab、全角空格、
	// 不间断空格、零宽空格等），避免用户输入时夹杂特殊空白导致无法识别。
	content := strings.Join(strings.FieldsFunc(body.Text.Content, isSpaceLike), "")
	if content == "" {
		return false
	}
	if !strings.Contains(content, triggerKeyword) {
		return false
	}

	// 收到包含“牛来”的文本后立即停止。企业微信不同版本的回调
	// 对 @ 信息字段格式并不一致，因此停止指令不再依赖 mention 字段。
	return true
}

func (nl *NiuLai) onTrigger() {
	if !nl.machine.StartScreaming() {
		return
	}

	nl.logger.Info("event triggered, start screaming")

	screamCtx := nl.startScreamCtx()

	nl.wg.Add(1)
	go func() {
		defer nl.wg.Done()
		nl.screamLoop(screamCtx)
	}()
}

// startScreamCtx 创建新的 SCREAMING 上下文，并取消旧的上下文
func (nl *NiuLai) startScreamCtx() context.Context {
	nl.screamMu.Lock()
	defer nl.screamMu.Unlock()

	if nl.screamCancel != nil {
		nl.screamCancel()
	}
	nl.screamCtx, nl.screamCancel = context.WithCancel(nl.ctx)
	return nl.screamCtx
}

// cancelScream 取消当前 SCREAMING 上下文
func (nl *NiuLai) cancelScream() {
	nl.screamMu.Lock()
	defer nl.screamMu.Unlock()
	if nl.screamCancel != nil {
		nl.screamCancel()
		nl.screamCancel = nil
	}
}

func (nl *NiuLai) screamLoop(ctx context.Context) {
	for {
		select {
		case <-nl.ctx.Done():
			return
		case <-ctx.Done():
			return
		default:
		}

		if nl.machine.Current() != state.SCREAMING {
			return
		}

		// 在真正发送前再检查一次停止信号，尽量减少停止后仍多发一轮的窗口
		select {
		case <-nl.ctx.Done():
			return
		case <-ctx.Done():
			return
		default:
		}

		nl.sendScreamToTargets(ctx)

		interval := nl.cfg.RandomInterval()
		nl.logger.Debug("next scream interval", "interval", interval)

		if !nl.sleep(ctx, interval) {
			return
		}
	}
}

// sendScreamToTargets 向所有目标群聊发送“妈妈”，并清理连续失败过多的群聊
func (nl *NiuLai) sendScreamToTargets(ctx context.Context) {
	chatIDs := nl.targetChatIDs()
	if len(chatIDs) == 0 {
		nl.logger.Warn("no target chats available, screaming will not send messages")
		return
	}

	for _, chatID := range chatIDs {
		select {
		case <-nl.ctx.Done():
			return
		case <-ctx.Done():
			return
		default:
		}

		if err := nl.client.SendMarkdown(chatID, wecom.ChatTypeGroup, screamContent); err != nil {
			failures := nl.recordSendFailure(chatID)
			nl.logger.Warn("failed to send scream message",
				"chatid", chatID,
				"err", err,
				"failures", failures,
				"max_failures", nl.cfg.MaxSendFailures,
			)
		} else {
			nl.resetSendFailures(chatID)
			nl.logger.Info("scream sent", "chatid", chatID, "content", screamContent)
		}
	}
}

func (nl *NiuLai) recordSendFailure(chatID string) int {
	nl.failuresMu.Lock()
	nl.failures[chatID]++
	failures := nl.failures[chatID]
	nl.failuresMu.Unlock()

	if failures >= nl.cfg.MaxSendFailures {
		nl.chatsMu.Lock()
		if _, ok := nl.chats[chatID]; ok {
			delete(nl.chats, chatID)
			nl.failuresMu.Lock()
			delete(nl.failures, chatID)
			nl.failuresMu.Unlock()
			nl.logger.Info("removed chat due to consecutive send failures",
				"chatid", chatID,
				"failures", failures,
				"max_failures", nl.cfg.MaxSendFailures,
			)
		}
		nl.chatsMu.Unlock()
	}

	return failures
}

func (nl *NiuLai) resetSendFailures(chatID string) {
	nl.failuresMu.Lock()
	defer nl.failuresMu.Unlock()
	delete(nl.failures, chatID)
}

func (nl *NiuLai) getSendFailures(chatID string) int {
	nl.failuresMu.Lock()
	defer nl.failuresMu.Unlock()
	return nl.failures[chatID]
}

// isSpaceLike 判断 r 是否为空格类字符，包括 Unicode 空白和零宽空格。
func isSpaceLike(r rune) bool {
	return unicode.IsSpace(r) || r == zeroWidthSpace
}
