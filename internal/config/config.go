package config

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
)

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

	TargetChatID string
	LogLevel     string
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

	logLevel := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL")))
	if logLevel == "" {
		logLevel = "info"
	}

	return &Config{
		WeComBotID:         botID,
		WeComBotSecret:     botSecret,
		WorkStartTime:      startTime,
		WorkEndTime:        endTime,
		WorkDays:           workDays,
		MinIntervalSeconds: minInterval,
		MaxIntervalSeconds: maxInterval,
		CooldownMinutes:    cooldown,
		MaxSendFailures:    maxSendFailures,
		TargetChatID:       strings.TrimSpace(os.Getenv("TARGET_CHAT_ID")),
		LogLevel:           logLevel,
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
