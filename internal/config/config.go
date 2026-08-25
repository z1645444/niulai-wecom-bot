package config

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"niulai-wecom-bot/internal/wecom"
)

// 完成后回复类型
const (
	// ReplyTypeText 回复纯文本
	ReplyTypeText = "text"
	// ReplyTypeImage 回复图片
	ReplyTypeImage = "image"
)

// DefaultFinishReplyText 是完成后回复的默认文本
const DefaultFinishReplyText = "🐮"

// DefaultStopKeyword 是停止指令的默认关键词
const DefaultStopKeyword = "牛来"

// DefaultScreamContent 是触发后喊话的默认内容
const DefaultScreamContent = "妈妈"

// zeroWidthSpace 与消息处理侧的规则一致：零宽空格按空白处理
const zeroWidthSpace = '\u200B'

// Config 保存应用运行时配置
type Config struct {
	WeComBotID     string
	WeComBotSecret string

	WorkStartTime time.Time
	WorkEndTime   time.Time
	WorkDays      map[int]struct{}

	MinIntervalSeconds int
	MaxIntervalSeconds int
	CooldownMinutes    int
	MaxSendFailures    int

	// ScreamContent 是触发后向目标群聊发送的喊话内容，默认“妈妈”
	ScreamContent string
	// ScreamVoiceFile 是触发后优先发送的语音文件绝对路径；
	// 文件缺失或上传失败时回退为发送 ScreamContent 文字
	ScreamVoiceFile string
	// StopKeyword 是停止指令关键词：文本剥离开头的 @提及 后包含该词即停止，默认“牛来”
	StopKeyword string

	// ForceTriggerWindowMinutes 是每日保底触发窗口：工作结束前该时长内
	// 若仍有群聊当天未触发，则强制触发一次
	ForceTriggerWindowMinutes int

	// FinishReplyType 是停止指令生效后的回复类型：text 或 image
	FinishReplyType string
	// FinishReplyText 是文本回复内容，默认 🐮
	FinishReplyText string
	// FinishReplyImageURL 是图片回复的地址索引，支持 http(s) URL 或本地文件路径
	FinishReplyImageURL string

	TargetChatID string
	LogLevel     string
}

// ParseTargetChatIDs parses the comma-separated TARGET_CHAT_ID value and
// returns trimmed, de-duplicated chat IDs in their original order.
func ParseTargetChatIDs(value string) []string {
	parts := strings.Split(value, ",")
	ids := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// FormatTargetChatIDs formats chat IDs for TARGET_CHAT_ID.
func FormatTargetChatIDs(ids []string) string {
	return strings.Join(ParseTargetChatIDs(strings.Join(ids, ",")), ",")
}

// normalizeSpaces 压缩字符串中的所有 Unicode 空白字符（含零宽空格），
// 与消息内容的空白压缩规则保持一致
func normalizeSpaces(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == zeroWidthSpace
	}), "")
}

// Load 从环境变量加载配置，返回解析后的 Config 或错误
func Load() (*Config, error) {
	botID := strings.TrimSpace(os.Getenv("WECOM_BOT_ID"))
	botSecret := strings.TrimSpace(os.Getenv("WECOM_BOT_SECRET"))

	if botID == "" {
		return nil, fmt.Errorf("WECOM_BOT_ID is required")
	}
	if botSecret == "" {
		return nil, fmt.Errorf("WECOM_BOT_SECRET is required")
	}

	startTime, err := parseTime(os.Getenv("WORK_START_TIME"), "09:00")
	if err != nil {
		return nil, fmt.Errorf("invalid WORK_START_TIME: %w", err)
	}
	endTime, err := parseTime(os.Getenv("WORK_END_TIME"), "18:00")
	if err != nil {
		return nil, fmt.Errorf("invalid WORK_END_TIME: %w", err)
	}

	workDays, err := parseWorkDays(os.Getenv("WORK_DAYS"), "1,2,3,4,5")
	if err != nil {
		return nil, fmt.Errorf("invalid WORK_DAYS: %w", err)
	}

	minInterval, err := parseInt(os.Getenv("MIN_INTERVAL_SECONDS"), 15)
	if err != nil {
		return nil, fmt.Errorf("invalid MIN_INTERVAL_SECONDS: %w", err)
	}
	maxInterval, err := parseInt(os.Getenv("MAX_INTERVAL_SECONDS"), 30)
	if err != nil {
		return nil, fmt.Errorf("invalid MAX_INTERVAL_SECONDS: %w", err)
	}
	if minInterval <= 0 {
		return nil, fmt.Errorf("MIN_INTERVAL_SECONDS must be positive")
	}
	if maxInterval < minInterval {
		return nil, fmt.Errorf("MAX_INTERVAL_SECONDS must be >= MIN_INTERVAL_SECONDS")
	}

	cooldown, err := parseInt(os.Getenv("COOLDOWN_MINUTES"), 120)
	if err != nil {
		return nil, fmt.Errorf("invalid COOLDOWN_MINUTES: %w", err)
	}
	if cooldown <= 0 {
		return nil, fmt.Errorf("COOLDOWN_MINUTES must be positive")
	}

	maxSendFailures, err := parseInt(os.Getenv("MAX_SEND_FAILURES"), 3)
	if err != nil {
		return nil, fmt.Errorf("invalid MAX_SEND_FAILURES: %w", err)
	}
	if maxSendFailures <= 0 {
		return nil, fmt.Errorf("MAX_SEND_FAILURES must be positive")
	}

	forceWindow, err := parseInt(os.Getenv("FORCE_TRIGGER_WINDOW_MINUTES"), 30)
	if err != nil {
		return nil, fmt.Errorf("invalid FORCE_TRIGGER_WINDOW_MINUTES: %w", err)
	}

	// 停止关键词按与消息内容相同的规则压缩空白，
	// 避免关键词本身含空白时永远无法命中压缩后的内容
	stopKeyword := normalizeSpaces(os.Getenv("STOP_KEYWORD"))
	if stopKeyword == "" {
		stopKeyword = DefaultStopKeyword
	}

	screamContent := strings.TrimSpace(os.Getenv("SCREAM_CONTENT"))
	if screamContent == "" {
		screamContent = DefaultScreamContent
	}

	voiceFile := strings.TrimSpace(os.Getenv("SCREAM_VOICE_FILE"))
	if voiceFile != "" {
		if !filepath.IsAbs(voiceFile) {
			return nil, fmt.Errorf("SCREAM_VOICE_FILE must be an absolute path, got %q", voiceFile)
		}
		// 企业微信语音素材仅支持 amr 格式、不超过 2MB；后缀与体积在启动时校验，
		// 避免配置错误导致每次喊话都静默回退为文字
		if !strings.HasSuffix(strings.ToLower(voiceFile), ".amr") {
			return nil, fmt.Errorf("SCREAM_VOICE_FILE must point to an .amr file, got %q", voiceFile)
		}
		// 文件不存在时不报错：保留运行时回退为文字喊话的行为
		if info, err := os.Stat(voiceFile); err == nil && info.Size() > wecom.MaxVoiceMediaSize {
			return nil, fmt.Errorf("SCREAM_VOICE_FILE %q too large: %d bytes, max %d", voiceFile, info.Size(), wecom.MaxVoiceMediaSize)
		}
	}

	replyType := strings.ToLower(strings.TrimSpace(os.Getenv("FINISH_REPLY_TYPE")))
	if replyType == "" {
		replyType = ReplyTypeText
	}
	if replyType != ReplyTypeText && replyType != ReplyTypeImage {
		return nil, fmt.Errorf("invalid FINISH_REPLY_TYPE %q: must be %q or %q", replyType, ReplyTypeText, ReplyTypeImage)
	}

	replyText := os.Getenv("FINISH_REPLY_TEXT")
	if strings.TrimSpace(replyText) == "" {
		replyText = DefaultFinishReplyText
	}

	replyImageURL := strings.TrimSpace(os.Getenv("FINISH_REPLY_IMAGE_URL"))
	if replyType == ReplyTypeImage && replyImageURL == "" {
		return nil, fmt.Errorf("FINISH_REPLY_IMAGE_URL is required when FINISH_REPLY_TYPE=%q", ReplyTypeImage)
	}

	logLevel := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL")))
	if logLevel == "" {
		logLevel = "info"
	}

	targetChatID := FormatTargetChatIDs([]string{os.Getenv("TARGET_CHAT_ID")})

	return &Config{
		WeComBotID:                botID,
		WeComBotSecret:            botSecret,
		WorkStartTime:             startTime,
		WorkEndTime:               endTime,
		WorkDays:                  workDays,
		MinIntervalSeconds:        minInterval,
		MaxIntervalSeconds:        maxInterval,
		CooldownMinutes:           cooldown,
		MaxSendFailures:           maxSendFailures,
		ScreamContent:             screamContent,
		ScreamVoiceFile:           voiceFile,
		StopKeyword:               stopKeyword,
		ForceTriggerWindowMinutes: forceWindow,
		FinishReplyType:           replyType,
		FinishReplyText:           replyText,
		FinishReplyImageURL:       replyImageURL,
		TargetChatID:              targetChatID,
		LogLevel:                  logLevel,
	}, nil
}

func parseTime(value, defaultValue string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		value = defaultValue
	}
	t, err := time.Parse("15:04", value)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

func parseWorkDays(value, defaultValue string) (map[int]struct{}, error) {
	if strings.TrimSpace(value) == "" {
		value = defaultValue
	}
	parts := strings.Split(value, ",")
	days := make(map[int]struct{}, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		d, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid day %q", p)
		}
		// 统一把 0 视为周日
		if d == 0 {
			d = 7
		}
		if d < 1 || d > 7 {
			return nil, fmt.Errorf("day %d out of range [0-7] or [1-7]", d)
		}
		days[d] = struct{}{}
	}
	if len(days) == 0 {
		return nil, fmt.Errorf("no valid work days")
	}
	return days, nil
}

func parseInt(value string, defaultValue int) (int, error) {
	if strings.TrimSpace(value) == "" {
		return defaultValue, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", value)
	}
	if n <= 0 {
		return 0, fmt.Errorf("must be positive, got %d", n)
	}
	return n, nil
}

// IsWorkTime 判断当前时间是否处于工作时间
// 注意：时间比较使用 time.Local，部署时请确保系统时区与预期一致
func (c *Config) IsWorkTime(now time.Time) bool {
	weekday := int(now.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	if _, ok := c.WorkDays[weekday]; !ok {
		return false
	}

	ref := time.Date(0, 1, 1, now.Hour(), now.Minute(), now.Second(), 0, time.Local)
	start := time.Date(0, 1, 1, c.WorkStartTime.Hour(), c.WorkStartTime.Minute(), 0, 0, time.Local)
	end := time.Date(0, 1, 1, c.WorkEndTime.Hour(), c.WorkEndTime.Minute(), 0, 0, time.Local)

	return !ref.Before(start) && ref.Before(end)
}

// RandomInterval 返回一个介于最小和最大间隔之间的随机持续时间
func (c *Config) RandomInterval() time.Duration {
	span := c.MaxIntervalSeconds - c.MinIntervalSeconds
	seconds := c.MinIntervalSeconds
	if span > 0 {
		seconds += rand.Intn(span + 1)
	}
	return time.Duration(seconds) * time.Second
}
