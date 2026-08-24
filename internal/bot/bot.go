package bot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
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

// imageHTTPClient 限定图片拉取的超时，避免远程地址不可用时拖住消息处理
var imageHTTPClient = &http.Client{Timeout: 15 * time.Second}

const (
	screamContent  = "妈妈"
	triggerKeyword = "牛来"
	zeroWidthSpace = '\u200B'
)

// Sender 是 bot 对外发送消息所需的最小接口，便于测试与解耦
type Sender interface {
	SendMarkdown(chatID string, chatType uint32, content string) error
	RespondMarkdown(reqID, content string) error
	RespondImage(reqID, mediaID string) error
	UploadMedia(ctx context.Context, mediaType, filename string, data []byte) (string, error)
}

// NiuLai 是虚拟员工“牛来”的业务编排器
type NiuLai struct {
	cfg       *config.Config
	machine   *state.Machine
	client    Sender
	scheduler *scheduler.Scheduler
	logger    *slog.Logger

	// chats 是运行时自动发现的群聊集合；发现到的 ID 会增量合并到
	// TARGET_CHAT_ID 配置中。
	chatsMu sync.RWMutex
	chats   map[string]struct{}

	// failures 记录每个 chatid 的连续发送失败次数，与是否自动发现无关
	failuresMu sync.Mutex
	failures   map[string]int

	// triggered 记录每个 chatid 最近一次触发成功的本地日期（yyyy-MM-dd），
	// 用于“每个群聊每天至少触发一次”的保底判断
	triggeredMu sync.Mutex
	triggered   map[string]string

	// mediaCache 缓存图片回复地址到 media_id 的映射，避免每次回复都重新上传素材
	mediaMu    sync.Mutex
	mediaCache map[string]string

	// screamCtx/screamCancel 控制当前 SCREAMING 会话的生命周期
	screamMu     sync.Mutex
	screamCtx    context.Context
	screamCancel context.CancelFunc

	// sleep 控制 screamLoop 的等待行为，便于测试注入
	sleep func(context.Context, time.Duration) bool

	// now 便于测试注入时间
	now func() time.Time

	running atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New 创建一个新的牛来实例
func New(cfg *config.Config, client Sender, logger *slog.Logger) *NiuLai {
	machine := state.NewMachine()
	nl := &NiuLai{
		cfg:        cfg,
		machine:    machine,
		client:     client,
		logger:     logger,
		chats:      make(map[string]struct{}),
		failures:   make(map[string]int),
		triggered:  make(map[string]string),
		mediaCache: make(map[string]string),
		sleep:      ctxSleep,
		now:        time.Now,
	}
	// 默认上下文，避免测试绕过 Start 时访问 nil ctx
	nl.ctx = context.Background()
	nl.scheduler = scheduler.New(cfg, machine, nl.onTrigger)
	nl.scheduler.SetPendingToday(nl.hasPendingChatToday)
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

	// 必须异步回复：图片回复中的 UploadMedia 需要等待 WS 响应帧，
	// 而响应帧只能由调用 OnMessage 的读循环读取；同步执行会堵死读循环直到超时
	go nl.sendFinishReply(reqID)
}

// sendFinishReply 在停止指令生效后回复该消息，回复内容由配置决定；
// 图片回复失败时回退为文本，保证完成信号可达
func (nl *NiuLai) sendFinishReply(reqID string) {
	if nl.client == nil {
		return
	}

	if nl.cfg.FinishReplyType == config.ReplyTypeImage {
		if err := nl.sendImageReply(reqID); err != nil {
			nl.logger.Warn("image finish reply failed, fallback to text", "err", err)
		} else {
			return
		}
	}

	if err := nl.client.RespondMarkdown(reqID, nl.finishReplyText()); err != nil {
		nl.logger.Warn("failed to send finish reply", "err", err)
	}
}

func (nl *NiuLai) finishReplyText() string {
	if strings.TrimSpace(nl.cfg.FinishReplyText) == "" {
		return config.DefaultFinishReplyText
	}
	return nl.cfg.FinishReplyText
}

// sendImageReply 上传图片素材并回复图片消息；失败时清除缓存以便下次重新上传
func (nl *NiuLai) sendImageReply(reqID string) error {
	mediaID, err := nl.finishImageMediaID()
	if err != nil {
		return err
	}
	if err := nl.client.RespondImage(reqID, mediaID); err != nil {
		nl.mediaMu.Lock()
		delete(nl.mediaCache, nl.cfg.FinishReplyImageURL)
		nl.mediaMu.Unlock()
		return err
	}
	return nil
}

// finishImageMediaID 返回图片回复的 media_id，进程内按地址缓存；
// media_id 有效期 3 天，远长于进程运行周期内的回复间隔，重启后自动重新上传
func (nl *NiuLai) finishImageMediaID() (string, error) {
	addr := nl.cfg.FinishReplyImageURL
	if addr == "" {
		return "", fmt.Errorf("FINISH_REPLY_IMAGE_URL is empty")
	}

	nl.mediaMu.Lock()
	mediaID, ok := nl.mediaCache[addr]
	nl.mediaMu.Unlock()
	if ok {
		return mediaID, nil
	}

	data, filename, err := fetchImage(addr)
	if err != nil {
		return "", err
	}

	mediaID, err = nl.client.UploadMedia(nl.ctx, "image", filename, data)
	if err != nil {
		return "", fmt.Errorf("upload finish reply image: %w", err)
	}

	nl.mediaMu.Lock()
	nl.mediaCache[addr] = mediaID
	nl.mediaMu.Unlock()
	nl.logger.Info("finish reply image uploaded", "addr", addr, "filename", filename)
	return mediaID, nil
}

// fetchImage 按地址读取图片内容：http(s) URL 走网络请求，其余按本地文件路径处理
func fetchImage(addr string) ([]byte, string, error) {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		resp, err := imageHTTPClient.Get(addr)
		if err != nil {
			return nil, "", fmt.Errorf("fetch image %q: %w", addr, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("fetch image %q: status %d", addr, resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, wecom.MaxImageMediaSize+1))
		if err != nil {
			return nil, "", fmt.Errorf("read image %q: %w", addr, err)
		}
		if len(data) > wecom.MaxImageMediaSize {
			return nil, "", fmt.Errorf("image %q exceeds %d bytes", addr, wecom.MaxImageMediaSize)
		}
		// 不信任服务端返回的 Content-Type 头，按内容嗅探，避免把错误页当图片上传
		if ct := http.DetectContentType(data); !strings.HasPrefix(ct, "image/") {
			return nil, "", fmt.Errorf("fetch image %q: unexpected content type %q", addr, ct)
		}
		filename := path.Base(resp.Request.URL.Path)
		if filename == "." || filename == "/" || filename == "" {
			filename = "image.png"
		}
		return data, filename, nil
	}

	data, err := os.ReadFile(addr)
	if err != nil {
		return nil, "", fmt.Errorf("read image file %q: %w", addr, err)
	}
	if len(data) > wecom.MaxImageMediaSize {
		return nil, "", fmt.Errorf("image %q exceeds %d bytes", addr, wecom.MaxImageMediaSize)
	}
	return data, filepath.Base(addr), nil
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

	configuredIDs := config.ParseTargetChatIDs(nl.cfg.TargetChatID)
	for _, id := range configuredIDs {
		if id == chatID {
			nl.logger.Debug("target chatid already configured", "chatid", chatID)
			return
		}
	}
	if _, ok := nl.chats[chatID]; ok {
		return
	}

	nl.chats[chatID] = struct{}{}
	configuredIDs = append(configuredIDs, chatID)
	nl.cfg.TargetChatID = config.FormatTargetChatIDs(configuredIDs)
	nl.logger.Info("auto-captured target group chatid", "chatid", chatID)
}

// targetChatIDs 返回当前需要发送消息的群聊 ID 列表
// TARGET_CHAT_ID 支持逗号分隔的多个 ID；自动发现的 ID 会被增量加入该配置。
func (nl *NiuLai) targetChatIDs() []string {
	nl.chatsMu.RLock()
	defer nl.chatsMu.RUnlock()

	return config.ParseTargetChatIDs(nl.cfg.TargetChatID)
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
			nl.markTriggered(chatID)
			nl.logger.Info("scream sent", "chatid", chatID, "content", screamContent)
		}
	}
}

// markTriggered 记录该群聊今天已经成功触发过事件
func (nl *NiuLai) markTriggered(chatID string) {
	nl.triggeredMu.Lock()
	defer nl.triggeredMu.Unlock()
	nl.triggered[chatID] = nl.now().Format("2006-01-02")
}

// hasPendingChatToday 报告当前目标群聊中是否仍有今天未触发过的群
func (nl *NiuLai) hasPendingChatToday() bool {
	today := nl.now().Format("2006-01-02")
	nl.triggeredMu.Lock()
	defer nl.triggeredMu.Unlock()
	for _, chatID := range nl.targetChatIDs() {
		if nl.triggered[chatID] != today {
			return true
		}
	}
	return false
}

func (nl *NiuLai) recordSendFailure(chatID string) int {
	nl.failuresMu.Lock()
	nl.failures[chatID]++
	failures := nl.failures[chatID]
	nl.failuresMu.Unlock()

	if failures >= nl.cfg.MaxSendFailures {
		nl.chatsMu.Lock()
		_, discovered := nl.chats[chatID]
		remaining := make([]string, 0)
		removedFromTargets := false
		for _, id := range config.ParseTargetChatIDs(nl.cfg.TargetChatID) {
			if id == chatID {
				removedFromTargets = true
				continue
			}
			remaining = append(remaining, id)
		}

		if discovered || removedFromTargets {
			if discovered {
				delete(nl.chats, chatID)
			}
			nl.cfg.TargetChatID = config.FormatTargetChatIDs(remaining)
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
