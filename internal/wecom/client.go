package wecom

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultWSURL      = "wss://openws.work.weixin.qq.com"
	heartbeatPeriod   = 30 * time.Second
	maxReconnectDelay = 60 * time.Second

	// callTimeout 是需要等待响应的请求（如素材上传）的超时时间
	callTimeout = 15 * time.Second

	// 临时素材上传限制：单分片编码前不超过 512KB，分片数不超过 100
	maxChunkSize = 512 * 1024
	maxChunks    = 100
	// maxChunkRetries 是单分片上传失败时的额外重试次数，与官方 SDK 行为一致
	maxChunkRetries = 2

	// MaxImageMediaSize 是图片素材的大小上限（10MB）
	MaxImageMediaSize = 10 * 1024 * 1024
	// MaxVoiceMediaSize 是语音素材的大小上限（2MB，仅支持 amr 格式）
	MaxVoiceMediaSize = 2 * 1024 * 1024
)

const (
	// ChatTypeGroup 是群聊的 chat_type 值
	ChatTypeGroup uint32 = 2
)

// Frame 是企业微信 WS 协议的通用消息帧
type Frame struct {
	Cmd     string          `json:"cmd,omitempty"`
	Headers Headers         `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`

	// 仅用于响应
	ErrCode int    `json:"errcode,omitempty"`
	ErrMsg  string `json:"errmsg,omitempty"`
}

// Headers 是通用请求头
type Headers struct {
	ReqID string `json:"req_id"`
}

// AuthBody 是 aibot_subscribe 的 body
type AuthBody struct {
	BotID  string `json:"bot_id"`
	Secret string `json:"secret"`
}

// SendMsgBody 是 aibot_send_msg 的 body
type SendMsgBody struct {
	ChatID   string    `json:"chatid"`
	ChatType uint32    `json:"chat_type,omitempty"`
	MsgType  string    `json:"msgtype"`
	Markdown *Markdown `json:"markdown,omitempty"`
	Voice    *Voice    `json:"voice,omitempty"`
}

// Markdown 是 markdown 消息体
type Markdown struct {
	Content string `json:"content"`
}

// Voice 是 voice 消息体
type Voice struct {
	MediaID string `json:"media_id"`
}

// Image 是 image 消息体
type Image struct {
	MediaID string `json:"media_id"`
}

// RespondMsgBody 是 aibot_respond_msg 的 body
type RespondMsgBody struct {
	MsgType  string    `json:"msgtype"`
	Markdown *Markdown `json:"markdown,omitempty"`
	Image    *Image    `json:"image,omitempty"`
}

// UploadMediaInitBody 是 aibot_upload_media_init 的 body
type UploadMediaInitBody struct {
	Type        string `json:"type"`
	Filename    string `json:"filename"`
	TotalSize   int    `json:"total_size"`
	TotalChunks int    `json:"total_chunks"`
	MD5         string `json:"md5,omitempty"`
}

// UploadMediaInitResp 是 aibot_upload_media_init 的响应 body
type UploadMediaInitResp struct {
	UploadID string `json:"upload_id"`
}

// UploadMediaChunkBody 是 aibot_upload_media_chunk 的 body
type UploadMediaChunkBody struct {
	UploadID   string `json:"upload_id"`
	ChunkIndex int    `json:"chunk_index"`
	Base64Data string `json:"base64_data"`
}

// UploadMediaFinishBody 是 aibot_upload_media_finish 的 body
type UploadMediaFinishBody struct {
	UploadID string `json:"upload_id"`
}

// UploadMediaFinishResp 是 aibot_upload_media_finish 的响应 body
type UploadMediaFinishResp struct {
	MediaID   string `json:"media_id"`
	Type      string `json:"type"`
	CreatedAt int64  `json:"created_at"`
}

// MsgCallbackBody 是 aibot_msg_callback 的 body
type MsgCallbackBody struct {
	MsgID    string    `json:"msgid"`
	AIBotID  string    `json:"aibotid"`
	ChatID   string    `json:"chatid"`
	ChatType string    `json:"chattype"`
	From     MsgFrom   `json:"from"`
	MsgType  string    `json:"msgtype"`
	Text     TextBody  `json:"text"`
	Mention  []Mention `json:"mention,omitempty"`
}

// EventCallbackBody 是 aibot_event_callback 的 body，仅提取群聊发现所需字段
type EventCallbackBody struct {
	ChatID   string `json:"chatid"`
	ChatType string `json:"chattype"`
}

// MsgFrom 是消息发送者
type MsgFrom struct {
	UserID string `json:"userid"`
}

// TextBody 是文本消息内容
type TextBody struct {
	Content string `json:"content"`
}

// Mention 是 @ 信息
type Mention struct {
	UserID string `json:"userid"`
	Type   int    `json:"type"`
}

// Handler 处理收到的消息回调
type Handler interface {
	// OnMessage 在收到用户消息时调用，reqID 用于被动回复
	OnMessage(reqID string, body *MsgCallbackBody)
	// OnEvent 在收到事件回调时调用，用于群聊发现等场景
	OnEvent(body *EventCallbackBody)
}

// Client 是企业微信智能机器人的 WebSocket 客户端
type Client struct {
	botID   string
	secret  string
	wsURL   string
	handler Handler
	logger  *slog.Logger

	conn   *websocket.Conn
	mu     sync.Mutex
	closed bool

	// sendCh 用于串行化出站帧，避免并发写 WebSocket
	sendCh chan Frame

	// pending 登记等待响应的出站请求，按 req_id 关联响应帧
	pendingMu sync.Mutex
	pending   map[string]chan Frame
}

// NewClient 创建一个新的 WS 客户端
func NewClient(botID, secret string, handler Handler, logger *slog.Logger) *Client {
	return &Client{
		botID:   botID,
		secret:  secret,
		wsURL:   defaultWSURL,
		handler: handler,
		logger:  logger,
		sendCh:  make(chan Frame, 64),
		pending: make(map[string]chan Frame),
	}
}

// SetURL 用于测试注入自定义 WS 地址
func (c *Client) SetURL(url string) *Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wsURL = url
	return c
}

// Run 启动连接、认证、心跳和消息读取循环，断线后自动重连
func (c *Client) Run(ctx context.Context) error {
	defer func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
	}()

	delay := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if delay > 0 {
			c.logger.Info("reconnecting after delay", "delay", delay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		served, err := c.connectAndServe(ctx)
		if err != nil {
			c.logger.Error("connection error", "err", err)
		}
		if served {
			// 连接成功服务过，重置退避
			delay = 0
		} else {
			delay = nextReconnectDelay(delay)
		}
	}
}

func (c *Client) connectAndServe(ctx context.Context) (bool, error) {
	c.logger.Info("connecting to wecom ws", "url", c.wsURL)
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.DialContext(ctx, c.wsURL, nil)
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		_ = conn.Close()
	}()

	// 父 context 取消时强制关闭连接，中断阻塞的 ReadJSON
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()
	go func() {
		select {
		case <-connCtx.Done():
			return
		case <-ctx.Done():
			_ = conn.Close()
		}
	}()

	// 发送认证帧
	authBody, err := json.Marshal(AuthBody{BotID: c.botID, Secret: c.secret})
	if err != nil {
		return false, fmt.Errorf("marshal auth body: %w", err)
	}
	authReq := Frame{
		Cmd:     "aibot_subscribe",
		Headers: Headers{ReqID: newReqID()},
		Body:    authBody,
	}

	if err := c.writeFrame(authReq); err != nil {
		return false, fmt.Errorf("send auth: %w", err)
	}

	// 读取认证响应
	var authResp Frame
	if err := conn.ReadJSON(&authResp); err != nil {
		return false, fmt.Errorf("read auth response: %w", err)
	}
	if authResp.ErrCode != 0 {
		return false, fmt.Errorf("auth failed: errcode=%d, errmsg=%s", authResp.ErrCode, authResp.ErrMsg)
	}
	c.logger.Info("authenticated")

	go c.sendLoop(connCtx)

	// 启动心跳 goroutine
	heartbeat := time.NewTicker(heartbeatPeriod)
	defer heartbeat.Stop()

	go func() {
		for {
			select {
			case <-connCtx.Done():
				return
			case <-heartbeat.C:
				if err := c.writeFrame(Frame{Cmd: "ping", Headers: Headers{ReqID: newReqID()}}); err != nil {
					c.logger.Warn("heartbeat failed", "err", err)
					connCancel()
					return
				}
			}
		}
	}()

	// 读取消息循环
	for {
		select {
		case <-connCtx.Done():
			return true, connCtx.Err()
		default:
		}

		var frame Frame
		if err := conn.ReadJSON(&frame); err != nil {
			return true, fmt.Errorf("read message: %w", err)
		}

		c.handleFrame(frame)
	}
}

func (c *Client) sendLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-c.sendCh:
			if !ok {
				return
			}
			if err := c.writeFrame(frame); err != nil {
				c.logger.Warn("send frame failed", "err", err)
			}
		}
	}
}

func (c *Client) writeFrame(frame Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	return c.conn.WriteJSON(frame)
}

func (c *Client) handleFrame(frame Frame) {
	// 响应帧（errcode/errmsg）按 req_id 分发给等待中的请求；
	// 响应帧可能没有 cmd，因此先按 pending 匹配再进入 cmd 分发
	if frame.Headers.ReqID != "" {
		c.pendingMu.Lock()
		ch, ok := c.pending[frame.Headers.ReqID]
		c.pendingMu.Unlock()
		if ok {
			select {
			case ch <- frame:
			default:
			}
			return
		}
	}

	// 无等待者的响应帧（来自 enqueue 的异步请求，如 aibot_respond_msg）：
	// 错误结果不应静默丢弃
	if frame.ErrCode != 0 {
		c.logger.Warn("server rejected request",
			"cmd", frame.Cmd,
			"req_id", frame.Headers.ReqID,
			"errcode", frame.ErrCode,
			"errmsg", frame.ErrMsg,
		)
		return
	}

	switch frame.Cmd {
	case "aibot_msg_callback":
		var body MsgCallbackBody
		if err := json.Unmarshal(frame.Body, &body); err != nil {
			c.logger.Warn("unmarshal msg callback", "err", err)
			return
		}
		if c.handler != nil {
			c.handler.OnMessage(frame.Headers.ReqID, &body)
		}
	case "aibot_event_callback":
		var body EventCallbackBody
		if err := json.Unmarshal(frame.Body, &body); err != nil {
			c.logger.Warn("unmarshal event callback", "err", err)
			return
		}
		c.logger.Debug("event callback received", "chatid", body.ChatID, "chattype", body.ChatType)
		if c.handler != nil {
			c.handler.OnEvent(&body)
		}
	case "pong", "":
		// 忽略心跳响应和裸响应帧
	default:
		c.logger.Debug("unknown frame", "cmd", frame.Cmd, "body", string(frame.Body))
	}
}

// SendMarkdown 主动发送 markdown 消息到指定会话
func (c *Client) SendMarkdown(ctx context.Context, chatID string, chatType uint32, content string) error {
	_, err := c.call(ctx, "aibot_send_msg", SendMsgBody{
		ChatID:   chatID,
		ChatType: chatType,
		MsgType:  "markdown",
		Markdown: &Markdown{Content: content},
	})
	return err
}

// SendVoice 主动发送语音消息到指定会话，mediaID 来自 UploadMedia
func (c *Client) SendVoice(ctx context.Context, chatID string, chatType uint32, mediaID string) error {
	_, err := c.call(ctx, "aibot_send_msg", SendMsgBody{
		ChatID:   chatID,
		ChatType: chatType,
		MsgType:  "voice",
		Voice:    &Voice{MediaID: mediaID},
	})
	return err
}

// RespondMarkdown 被动回复 markdown 消息（回复 aibot_msg_callback）
func (c *Client) RespondMarkdown(reqID, content string) error {
	return c.enqueue("aibot_respond_msg", reqID, RespondMsgBody{
		MsgType:  "markdown",
		Markdown: &Markdown{Content: content},
	})
}

// RespondImage 被动回复图片消息，mediaID 来自 UploadMedia
func (c *Client) RespondImage(reqID, mediaID string) error {
	return c.enqueue("aibot_respond_msg", reqID, RespondMsgBody{
		MsgType: "image",
		Image:   &Image{MediaID: mediaID},
	})
}

// UploadMedia 通过 init/chunk/finish 三步上传临时素材，返回 media_id（3 天内有效）
func (c *Client) UploadMedia(ctx context.Context, mediaType, filename string, data []byte) (string, error) {
	if len(data) < 5 {
		return "", fmt.Errorf("media too small: %d bytes", len(data))
	}
	if mediaType == "image" && len(data) > MaxImageMediaSize {
		return "", fmt.Errorf("image too large: %d bytes, max %d", len(data), MaxImageMediaSize)
	}
	if mediaType == "voice" && len(data) > MaxVoiceMediaSize {
		return "", fmt.Errorf("voice too large: %d bytes, max %d", len(data), MaxVoiceMediaSize)
	}
	totalChunks := (len(data) + maxChunkSize - 1) / maxChunkSize
	if totalChunks > maxChunks {
		return "", fmt.Errorf("media requires %d chunks, max %d", totalChunks, maxChunks)
	}

	md5Sum := md5.Sum(data)
	initResp, err := c.call(ctx, "aibot_upload_media_init", UploadMediaInitBody{
		Type:        mediaType,
		Filename:    filename,
		TotalSize:   len(data),
		TotalChunks: totalChunks,
		MD5:         hex.EncodeToString(md5Sum[:]),
	})
	if err != nil {
		return "", err
	}
	var init UploadMediaInitResp
	if err := json.Unmarshal(initResp.Body, &init); err != nil {
		return "", fmt.Errorf("unmarshal upload init response: %w", err)
	}
	if init.UploadID == "" {
		return "", fmt.Errorf("upload init returned empty upload_id")
	}

	for i := 0; i < totalChunks; i++ {
		start := i * maxChunkSize
		end := start + maxChunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := UploadMediaChunkBody{
			UploadID:   init.UploadID,
			ChunkIndex: i,
			Base64Data: base64.StdEncoding.EncodeToString(data[start:end]),
		}
		// 单分片失败做有限重试，避免一次抖动导致整个上传失败
		var err error
		for attempt := 0; attempt <= maxChunkRetries; attempt++ {
			if _, err = c.call(ctx, "aibot_upload_media_chunk", chunk); err == nil {
				break
			}
		}
		if err != nil {
			return "", err
		}
	}

	finishResp, err := c.call(ctx, "aibot_upload_media_finish", UploadMediaFinishBody{UploadID: init.UploadID})
	if err != nil {
		return "", err
	}
	var finish UploadMediaFinishResp
	if err := json.Unmarshal(finishResp.Body, &finish); err != nil {
		return "", fmt.Errorf("unmarshal upload finish response: %w", err)
	}
	if finish.MediaID == "" {
		return "", fmt.Errorf("upload finish returned empty media_id")
	}
	return finish.MediaID, nil
}

// call 发送一个需要等待响应的请求帧，按 req_id 关联响应
func (c *Client) call(ctx context.Context, cmd string, body any) (Frame, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return Frame{}, fmt.Errorf("marshal %s body: %w", cmd, err)
	}

	reqID := newReqID()
	ch := make(chan Frame, 1)
	c.pendingMu.Lock()
	c.pending[reqID] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, reqID)
		c.pendingMu.Unlock()
	}()

	if err := c.writeFrame(Frame{Cmd: cmd, Headers: Headers{ReqID: reqID}, Body: raw}); err != nil {
		return Frame{}, fmt.Errorf("send %s: %w", cmd, err)
	}

	timer := time.NewTimer(callTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case <-timer.C:
		return Frame{}, fmt.Errorf("%s: response timeout", cmd)
	case resp := <-ch:
		if resp.ErrCode != 0 {
			return Frame{}, fmt.Errorf("%s: errcode=%d, errmsg=%s", cmd, resp.ErrCode, resp.ErrMsg)
		}
		return resp, nil
	}
}

// enqueue 序列化 body 并把帧送入出站队列（异步，不等待对端响应）
func (c *Client) enqueue(cmd, reqID string, body any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("client closed")
	}
	if c.conn == nil {
		c.mu.Unlock()
		return fmt.Errorf("not connected")
	}
	c.mu.Unlock()

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s body: %w", cmd, err)
	}

	select {
	case c.sendCh <- Frame{
		Cmd:     cmd,
		Headers: Headers{ReqID: reqID},
		Body:    raw,
	}:
		return nil
	default:
		return fmt.Errorf("send channel full")
	}
}

func nextReconnectDelay(current time.Duration) time.Duration {
	if current == 0 {
		return time.Second
	}
	next := current * 2
	if next > maxReconnectDelay {
		return maxReconnectDelay
	}
	return next
}

func newReqID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
