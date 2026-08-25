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
	"slices"
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

const zeroWidthSpace = '\u200B'

// Sender 是 bot 对外发送消息所需的最小接口，便于测试与解耦
type Sender interface {
	SendMarkdown(ctx context.Context, chatID string, chatType uint32, content string) error
	SendVoice(ctx context.Context, chatID string, chatType uint32, mediaID string) error
	RespondMarkdown(reqID, content string) error
	RespondImage(reqID, mediaID string) error
	UploadMedia(ctx context.Context, mediaType, filename string, data []byte) (string, error)
}

// NiuLai 是虚拟员工“牛来”的业务编排器
type NiuLai struct {
	cfg       *config.Config
	client    Sender
	scheduler *scheduler.Scheduler
	logger    *slog.Logger

	// chats 是运行时自动发现的群聊集合；发现到的 ID 会增量合并到
	// TARGET_CHAT_ID 配置中。
	chatsMu sync.RWMutex
	chats   map[string]struct{}

	// sessions 按群聊维护相互独立的会话（状态机与 SCREAMING 生命周期）：
	// 一个群的触发、停止与冷却不影响其他群
	sessionsMu sync.Mutex
	sessions   map[string]*chatSession

	// failures 记录每个 chatid 的连续发送失败次数，与是否自动发现无关
	failuresMu sync.Mutex
	failures   map[string]int

	// triggered 记录每个 chatid 最近一次触发成功的本地日期（yyyy-MM-dd），
	// 用于“每个群聊每天至少触发一次”的保底判断
	triggeredMu sync.Mutex
	triggered   map[string]string

	// mediaCache 缓存素材地址（图片地址、语音文件路径）到已上传素材的映射，
	// 避免每次发送都重新上传素材
	mediaMu    sync.Mutex
	mediaCache map[string]mediaEntry

	// voiceFailures 记录语音喊话的连续失败次数；达到 maxVoiceFailures 后在
	// voiceDisabledUntil 之前暂停语音尝试（期间直接回退文字），避免服务端
	// 持续拒绝语音时每个喊话周期都重新上传素材
	voiceMu            sync.Mutex
	voiceFailures      int
	voiceDisabledUntil time.Time

	// persistTargets 在目标群聊列表变化（自动发现、失败移除）后持久化最新的
	// TARGET_CHAT_ID 值（如回写 .env）；为 nil 时不持久化。须在 Start 前完成设置
	persistTargets func(value string) error

	// sleep 控制 screamLoop 的等待行为，便于测试注入
	sleep func(context.Context, time.Duration) bool

	// now 便于测试注入时间
	now func() time.Time

	running atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// mediaIDRefreshAfter 是缓存 media_id 的刷新阈值：企业微信临时素材有效期 3 天，
// 超过该时长即视为过期并重新上传，留出充足余量避免用到临期 media_id
const mediaIDRefreshAfter = 48 * time.Hour

const (
	// maxVoiceFailures 是语音喊话连续失败的容忍次数，达到后进入暂停期
	maxVoiceFailures = 3
	// voiceDisableDuration 是语音连续失败后的暂停时长，到期自动重试
	voiceDisableDuration = time.Hour
)

// mediaEntry 是已上传素材的缓存条目
type mediaEntry struct {
	mediaID    string
	uploadedAt time.Time
	// size 与 modTime 仅对本地文件素材（语音）记录，用于检测同路径文件被替换；
	// 非文件来源（图片 URL）保持零值
	size    int64
	modTime time.Time
}

// cachedMediaID 返回缓存且未过期的 mediaID；缺失、超过刷新阈值，
// 或文件信息（info 非 nil 时）与缓存不一致（同路径文件被替换）时返回 false
func (nl *NiuLai) cachedMediaID(addr string, info os.FileInfo) (string, bool) {
	nl.mediaMu.Lock()
	defer nl.mediaMu.Unlock()
	entry, ok := nl.mediaCache[addr]
	if !ok || nl.now().Sub(entry.uploadedAt) >= mediaIDRefreshAfter {
		return "", false
	}
	if info != nil && (entry.size != info.Size() || !entry.modTime.Equal(info.ModTime())) {
		return "", false
	}
	return entry.mediaID, true
}

// storeMediaID 缓存上传成功的 mediaID，并记录上传时间用于过期判断；
// info 非 nil 时记录文件信息，用于检测同路径文件被替换
func (nl *NiuLai) storeMediaID(addr, mediaID string, info os.FileInfo) {
	nl.mediaMu.Lock()
	defer nl.mediaMu.Unlock()
	entry := mediaEntry{mediaID: mediaID, uploadedAt: nl.now()}
	if info != nil {
		entry.size = info.Size()
		entry.modTime = info.ModTime()
	}
	nl.mediaCache[addr] = entry
}

// evictMediaID 清除缓存的 mediaID，发送失败后下次重新上传
func (nl *NiuLai) evictMediaID(addr string) {
	nl.mediaMu.Lock()
	defer nl.mediaMu.Unlock()
	delete(nl.mediaCache, addr)
}

// voiceDisabled 报告语音喊话是否处于连续失败后的暂停期
func (nl *NiuLai) voiceDisabled() bool {
	nl.voiceMu.Lock()
	defer nl.voiceMu.Unlock()
	return nl.now().Before(nl.voiceDisabledUntil)
}

// recordVoiceFailure 累计语音喊话连续失败次数，达到阈值后暂停语音一段时间
func (nl *NiuLai) recordVoiceFailure() {
	nl.voiceMu.Lock()
	defer nl.voiceMu.Unlock()
	nl.voiceFailures++
	if nl.voiceFailures < maxVoiceFailures {
		return
	}
	nl.voiceFailures = 0
	nl.voiceDisabledUntil = nl.now().Add(voiceDisableDuration)
	nl.logger.Warn("voice scream paused after consecutive failures, falling back to text",
		"failures", maxVoiceFailures,
		"resume_at", nl.voiceDisabledUntil.Format(time.RFC3339),
	)
}

// resetVoiceFailures 在语音喊话成功后清零连续失败计数
func (nl *NiuLai) resetVoiceFailures() {
	nl.voiceMu.Lock()
	defer nl.voiceMu.Unlock()
	nl.voiceFailures = 0
}

// chatSession 是单个群聊的独立会话：状态机与 SCREAMING 上下文均按群隔离
type chatSession struct {
	machine *state.Machine

	// mu 保护 cancel，即当前 SCREAMING 循环的取消函数
	mu     sync.Mutex
	cancel context.CancelFunc
}

// startScreamCtx 创建该会话新的 SCREAMING 上下文，并取消旧上下文
func (s *chatSession) startScreamCtx(parent context.Context) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	return ctx
}

// cancelScream 取消该会话当前的 SCREAMING 上下文
func (s *chatSession) cancelScream() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// New 创建一个新的牛来实例
func New(cfg *config.Config, client Sender, logger *slog.Logger) *NiuLai {
	// 空值回退默认，保证绕过 config.Load 的调用方（如测试）行为确定
	if cfg.StopKeyword == "" {
		cfg.StopKeyword = config.DefaultStopKeyword
	}
	if cfg.ScreamContent == "" {
		cfg.ScreamContent = config.DefaultScreamContent
	}
	nl := &NiuLai{
		cfg:        cfg,
		client:     client,
		logger:     logger,
		chats:      make(map[string]struct{}),
		sessions:   make(map[string]*chatSession),
		failures:   make(map[string]int),
		triggered:  make(map[string]string),
		mediaCache: make(map[string]mediaEntry),
		sleep:      ctxSleep,
		now:        time.Now,
	}
	// 默认上下文，避免测试绕过 Start 时访问 nil ctx
	nl.ctx = context.Background()
	nl.scheduler = scheduler.New(cfg, nl.targetChatIDs, nl.machineFor, nl.onTrigger)
	nl.scheduler.SetPendingToday(nl.hasPendingChat)
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
	nl.cancelAllScreams()
	nl.wg.Wait()
	nl.logger.Info("niulai stopped")
}

// SetClient 注入 WebSocket 客户端，解决初始化时的循环依赖
func (nl *NiuLai) SetClient(client Sender) {
	nl.client = client
}

// SetTargetChatIDsPersister 注册目标群聊列表的持久化回调，
// 在自动发现新群聊或群聊被移出目标列表时调用
func (nl *NiuLai) SetTargetChatIDsPersister(fn func(value string) error) {
	nl.persistTargets = fn
}

// persistTargetChatIDsLocked 把当前 TARGET_CHAT_ID 交给持久化回调，失败仅记日志。
// 调用方必须持有 chatsMu：写回顺序与配置变更顺序一致，
// 避免并发变更下旧值覆盖新值
func (nl *NiuLai) persistTargetChatIDsLocked() {
	if nl.persistTargets == nil {
		return
	}
	if err := nl.persistTargets(nl.cfg.TargetChatID); err != nil {
		nl.logger.Warn("failed to persist target chat ids", "err", err)
	}
}

// OnMessage 实现 wecom.Handler，处理收到的用户消息。
// 文本剥离开头的 @提及 后仍包含停止关键词（默认“牛来”）即触发停止，
// 不依赖企业微信回调中的 @ 字段。
// 停止只作用于消息来源的群聊：该群进入冷却，其他群的 SCREAMING 不受影响。
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

	session := nl.findSession(body.ChatID)
	if session == nil {
		nl.logger.Info("stop command ignored: chat has no session", "chatid", body.ChatID)
		return
	}

	cooldown := time.Duration(nl.cfg.CooldownMinutes) * time.Minute
	if !session.machine.StopScreaming(cooldown) {
		nl.logger.Info("stop command ignored: not screaming",
			"state", session.machine.Current(),
			"chatid", body.ChatID,
		)
		return
	}

	session.cancelScream()

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
		nl.evictMediaID(nl.cfg.FinishReplyImageURL)
		return err
	}
	return nil
}

// finishImageMediaID 返回图片回复的 media_id，进程内按地址缓存，到期自动重新上传
func (nl *NiuLai) finishImageMediaID() (string, error) {
	addr := nl.cfg.FinishReplyImageURL
	if addr == "" {
		return "", fmt.Errorf("FINISH_REPLY_IMAGE_URL is empty")
	}

	if mediaID, ok := nl.cachedMediaID(addr, nil); ok {
		return mediaID, nil
	}

	data, filename, err := fetchImage(addr)
	if err != nil {
		return "", err
	}

	mediaID, err := nl.client.UploadMedia(nl.ctx, "image", filename, data)
	if err != nil {
		return "", fmt.Errorf("upload finish reply image: %w", err)
	}

	nl.storeMediaID(addr, mediaID, nil)
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
	nl.persistTargetChatIDsLocked()
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

	// 1. 泛化提及剥离（作用于原文）：客户端插入的提及以 @ 开始、以空白类字符
	// 结束并分隔后续内容，循环剥离开头连续的提及段；无分隔符的“@xxx”无法
	// 确定名字边界，交给第 3 步的已知前缀兜底
	content := stripLeadingMentions(body.Text.Content)
	// 2. 去除所有 Unicode 空白字符（包括普通空格、Tab、全角空格、
	// 不间断空格、零宽空格等），避免用户输入时夹杂特殊空白导致无法识别。
	// 必须放在提及剥离之后，否则分隔符被抹掉、提及边界丢失
	content = strings.Join(strings.FieldsFunc(content, isSpaceLike), "")
	// 3. 已知关键词前缀兜底：处理“@牛来不要停”（无分隔符）和“@牛来”（纯提及）
	content = strings.TrimPrefix(content, "@"+nl.cfg.StopKeyword)
	if content == "" {
		return false
	}

	// 4. 剩余内容包含关键词即停止。企业微信不同版本的回调
	// 对 @ 信息字段格式并不一致，因此停止指令不依赖 mention 字段。
	return strings.Contains(content, nl.cfg.StopKeyword)
}

// stripLeadingMentions 循环剥离文本开头连续的 @提及 段：
// 提及以 @ 开始、以空白类字符（含零宽空格）为分隔符；遇到无分隔符的 @ 段即返回，
// 由调用方用已知关键词前缀兜底
func stripLeadingMentions(s string) string {
	for strings.HasPrefix(s, "@") {
		idx := strings.IndexFunc(s, isSpaceLike)
		if idx < 0 {
			return s
		}
		s = strings.TrimLeftFunc(s[idx:], isSpaceLike)
	}
	return s
}

// sessionFor 返回指定群聊的会话，不存在时创建（初始为 IDLE）
func (nl *NiuLai) sessionFor(chatID string) *chatSession {
	nl.sessionsMu.Lock()
	defer nl.sessionsMu.Unlock()

	session, ok := nl.sessions[chatID]
	if !ok {
		session = &chatSession{machine: state.NewMachine()}
		nl.sessions[chatID] = session
	}
	return session
}

// findSession 返回指定群聊的已有会话，不存在时返回 nil（不创建）
func (nl *NiuLai) findSession(chatID string) *chatSession {
	nl.sessionsMu.Lock()
	defer nl.sessionsMu.Unlock()
	return nl.sessions[chatID]
}

// machineFor 返回指定群聊的独立状态机，供调度器按群判定触发
func (nl *NiuLai) machineFor(chatID string) *state.Machine {
	return nl.sessionFor(chatID).machine
}

func (nl *NiuLai) onTrigger(chatID string) {
	// 读锁贯穿成员校验、会话创建与 SCREAMING 上下文建立，使失败驱逐无法
	// 交错进来：要么校验时群已被移出目标列表、放弃触发；要么驱逐晚于上下文
	// 建立，由 evictSession 正常取消本次循环。否则调度 tick 的快照与驱逐
	// 交错时可能为已驱逐的群重建会话，启动无法再被驱逐的发送循环
	nl.chatsMu.RLock()
	defer nl.chatsMu.RUnlock()

	// 不能直接调 targetChatIDs()：已持有读锁，Go 的 RWMutex 读锁不可重入，
	// 一旦有写者等待，再次 RLock 会死锁
	if !slices.Contains(config.ParseTargetChatIDs(nl.cfg.TargetChatID), chatID) {
		nl.logger.Warn("trigger ignored: chat is no longer a target", "chatid", chatID)
		return
	}

	session := nl.sessionFor(chatID)
	if !session.machine.StartScreaming() {
		return
	}

	nl.logger.Info("event triggered, start screaming", "chatid", chatID)

	screamCtx := session.startScreamCtx(nl.ctx)

	nl.wg.Add(1)
	go func() {
		defer nl.wg.Done()
		nl.screamLoop(screamCtx, session, chatID)
	}()
}

// cancelAllScreams 取消所有群聊的 SCREAMING 上下文
func (nl *NiuLai) cancelAllScreams() {
	nl.sessionsMu.Lock()
	sessions := make([]*chatSession, 0, len(nl.sessions))
	for _, session := range nl.sessions {
		sessions = append(sessions, session)
	}
	nl.sessionsMu.Unlock()

	for _, session := range sessions {
		session.cancelScream()
	}
}

// evictSession 终止并删除指定群聊的会话：被移出目标列表的群不再发送，
// 之后若被重新发现，将以全新的 IDLE 会话参与触发
func (nl *NiuLai) evictSession(chatID string) {
	nl.sessionsMu.Lock()
	session, ok := nl.sessions[chatID]
	if ok {
		delete(nl.sessions, chatID)
	}
	nl.sessionsMu.Unlock()

	if ok {
		session.cancelScream()
	}
}

func (nl *NiuLai) screamLoop(ctx context.Context, session *chatSession, chatID string) {
	for {
		select {
		case <-nl.ctx.Done():
			return
		case <-ctx.Done():
			return
		default:
		}

		if session.machine.Current() != state.SCREAMING {
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

		nl.sendScream(chatID)

		interval := nl.cfg.RandomInterval()
		nl.logger.Debug("next scream interval", "chatid", chatID, "interval", interval)

		if !nl.sleep(ctx, interval) {
			return
		}
	}
}

// sendScream 向指定群聊发送一次喊话内容；连续失败过多的群聊会被移出目标列表。
// 配置了语音文件时优先发送语音；语音不可用（文件缺失、读取/上传/发送失败，
// 或处于连续失败暂停期）时回退为文字
func (nl *NiuLai) sendScream(chatID string) {
	if nl.cfg.ScreamVoiceFile != "" && !nl.voiceDisabled() {
		if err := nl.sendVoiceScream(chatID); err != nil {
			nl.recordVoiceFailure()
			nl.logger.Warn("voice scream failed, fallback to text",
				"chatid", chatID,
				"file", nl.cfg.ScreamVoiceFile,
				"err", err,
			)
		} else {
			nl.resetVoiceFailures()
			nl.resetSendFailures(chatID)
			nl.markTriggered(chatID)
			nl.logger.Info("voice scream sent", "chatid", chatID, "file", nl.cfg.ScreamVoiceFile)
			return
		}
	}

	if err := nl.client.SendMarkdown(nl.ctx, chatID, wecom.ChatTypeGroup, nl.cfg.ScreamContent); err != nil {
		failures := nl.recordSendFailure(chatID)
		nl.logger.Warn("failed to send scream message",
			"chatid", chatID,
			"err", err,
			"failures", failures,
			"max_failures", nl.cfg.MaxSendFailures,
		)
		return
	}
	nl.resetSendFailures(chatID)
	nl.markTriggered(chatID)
	nl.logger.Info("scream sent", "chatid", chatID, "content", nl.cfg.ScreamContent)
}

// sendVoiceScream 上传语音素材并向指定群聊发送语音消息；
// 发送失败时清除缓存的 media_id 以便下次重新上传
func (nl *NiuLai) sendVoiceScream(chatID string) error {
	mediaID, err := nl.screamVoiceMediaID()
	if err != nil {
		return err
	}
	if err := nl.client.SendVoice(nl.ctx, chatID, wecom.ChatTypeGroup, mediaID); err != nil {
		nl.evictMediaID(nl.cfg.ScreamVoiceFile)
		return err
	}
	return nil
}

// screamVoiceMediaID 返回喊话语音的 media_id，进程内按文件路径缓存，
// 超过刷新阈值或同路径文件被替换后自动重新上传。
// 每次发送前检测文件是否存在，文件缺失时由调用方回退为文字
func (nl *NiuLai) screamVoiceMediaID() (string, error) {
	addr := nl.cfg.ScreamVoiceFile
	if addr == "" {
		return "", fmt.Errorf("SCREAM_VOICE_FILE is empty")
	}
	info, err := os.Stat(addr)
	if err != nil {
		return "", fmt.Errorf("voice file %q: %w", addr, err)
	}
	// 企业微信语音素材上限 2MB：超限必然上传失败，在发送前直接交由调用方回退文字
	if info.Size() > wecom.MaxVoiceMediaSize {
		return "", fmt.Errorf("voice file %q too large: %d bytes, max %d", addr, info.Size(), wecom.MaxVoiceMediaSize)
	}

	if mediaID, ok := nl.cachedMediaID(addr, info); ok {
		return mediaID, nil
	}

	data, err := os.ReadFile(addr)
	if err != nil {
		return "", fmt.Errorf("read voice file %q: %w", addr, err)
	}

	mediaID, err := nl.client.UploadMedia(nl.ctx, "voice", filepath.Base(addr), data)
	if err != nil {
		return "", fmt.Errorf("upload scream voice: %w", err)
	}

	nl.storeMediaID(addr, mediaID, info)
	nl.logger.Info("scream voice uploaded", "file", addr)
	return mediaID, nil
}

// markTriggered 记录该群聊今天已经成功触发过事件
func (nl *NiuLai) markTriggered(chatID string) {
	nl.triggeredMu.Lock()
	defer nl.triggeredMu.Unlock()
	nl.triggered[chatID] = nl.now().Format("2006-01-02")
}

// hasPendingChat 报告指定群聊当天是否尚未成功触发
func (nl *NiuLai) hasPendingChat(chatID string) bool {
	today := nl.now().Format("2006-01-02")
	nl.triggeredMu.Lock()
	defer nl.triggeredMu.Unlock()
	return nl.triggered[chatID] != today
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
			nl.persistTargetChatIDsLocked()
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

		if discovered || removedFromTargets {
			nl.evictSession(chatID)
		}
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
