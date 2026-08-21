package wecom

import (
	"context"
	"crypto/rand"
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
	ChatID   string   `json:"chatid"`
	ChatType uint32   `json:"chat_type,omitempty"`
	MsgType  string   `json:"msgtype"`
	Markdown Markdown `json:"markdown"`
}

// Markdown 是 markdown 消息体
type Markdown struct {
	Content string `json:"content"`
}

// RespondMsgBody 是 aibot_respond_msg 的 body
type RespondMsgBody struct {
	MsgType  string   `json:"msgtype"`
	Markdown Markdown `json:"markdown"`
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
func (c *Client) SendMarkdown(chatID string, chatType uint32, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("client closed")
	}
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	body, err := json.Marshal(SendMsgBody{
		ChatID:   chatID,
		ChatType: chatType,
		MsgType:  "markdown",
		Markdown: Markdown{Content: content},
	})
	if err != nil {
		return fmt.Errorf("marshal send body: %w", err)
	}

	select {
	case c.sendCh <- Frame{
		Cmd:     "aibot_send_msg",
		Headers: Headers{ReqID: newReqID()},
		Body:    body,
	}:
		return nil
	default:
		return fmt.Errorf("send channel full")
	}
}

// RespondMarkdown 被动回复 markdown 消息（回复 aibot_msg_callback）
func (c *Client) RespondMarkdown(reqID, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("client closed")
	}
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}

	body, err := json.Marshal(RespondMsgBody{
		MsgType:  "markdown",
		Markdown: Markdown{Content: content},
	})
	if err != nil {
		return fmt.Errorf("marshal respond body: %w", err)
	}

	select {
	case c.sendCh <- Frame{
		Cmd:     "aibot_respond_msg",
		Headers: Headers{ReqID: reqID},
		Body:    body,
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
