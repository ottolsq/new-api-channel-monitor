package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config 是监控工具的全局配置。
// 加载顺序：代码内默认值 → 工作目录 .env（KEY=VALUE）→ 进程环境变量覆盖。
// 真正的环境变量（export / docker -e / systemd Environment）优先级最高，不会被 .env 覆盖。
type Config struct {
	APIBaseURL           string
	DashboardUsername    string
	DashboardPassword    string
	FeishuWebhookURL     string
	DBPath               string
	IntervalMinutes      int
	RetryTimes           int
	RetryDelaySeconds    int
	TimeoutSeconds       int
	AlertDebounceMinutes int
	LogRetentionDays     int
}

const defaultEnvPath = ".env"

// defaultConfig 返回默认配置。
func defaultConfig() Config {
	return Config{
		APIBaseURL:           "http://localhost:3000",
		DBPath:               defaultDBPath(),
		IntervalMinutes:      30,
		RetryTimes:           2,
		RetryDelaySeconds:    30,
		TimeoutSeconds:       60,
		AlertDebounceMinutes: 60,
	}
}

// defaultDBPath 返回默认数据库路径，位于可执行文件同目录。
func defaultDBPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "monitor.db"
	}
	return filepath.Join(filepath.Dir(exe), "monitor.db")
}

// loadConfig 加载配置：先注入工作目录下的 .env（不覆盖已有环境变量），
// 再用环境变量覆盖默认值，最后校验必填项。
func loadConfig() (Config, error) {
	loadDotEnv(defaultEnvPath)

	cfg := defaultConfig()
	applyEnvOverrides(&cfg)

	if err := validateConfig(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// loadDotEnv 读取 KEY=VALUE 格式的 .env 文件，将其中「尚未在进程环境中设置」的变量注入。
// 已通过 export / docker -e / systemd Environment 设置的环境变量优先，不会被 .env 覆盖。
// 文件不存在时静默跳过；不引第三方依赖，仅处理 # 注释、可选引号与首尾空白。
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}

// applyEnvOverrides 用非空环境变量覆盖 cfg 字段。
// 字符串字段直接覆盖；整数字段按 strconv 解析，解析失败时回退到 cfg 已有值。
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("API_BASE_URL"); v != "" {
		cfg.APIBaseURL = v
	}
	if v := os.Getenv("DASHBOARD_USERNAME"); v != "" {
		cfg.DashboardUsername = v
	}
	if v := os.Getenv("DASHBOARD_PASSWORD"); v != "" {
		cfg.DashboardPassword = v
	}
	if v := os.Getenv("FEISHU_WEBHOOK_URL"); v != "" {
		cfg.FeishuWebhookURL = v
	}
	if v := os.Getenv("DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v, ok := parseIntEnv("INTERVAL_MINUTES"); ok {
		cfg.IntervalMinutes = v
	}
	if v, ok := parseIntEnv("RETRY_TIMES"); ok {
		cfg.RetryTimes = v
	}
	if v, ok := parseIntEnv("RETRY_DELAY_SECONDS"); ok {
		cfg.RetryDelaySeconds = v
	}
	if v, ok := parseIntEnv("TIMEOUT_SECONDS"); ok {
		cfg.TimeoutSeconds = v
	}
	if v, ok := parseIntEnv("ALERT_DEBOUNCE_MINUTES"); ok {
		cfg.AlertDebounceMinutes = v
	}
	if v, ok := parseIntEnv("LOG_RETENTION_DAYS"); ok {
		cfg.LogRetentionDays = v
	}
}

// parseIntEnv 读取命名环境变量并解析为 int。
// 空值或解析失败时返回 (0, false)，调用方据此判断是否覆盖已有值。
func parseIntEnv(name string) (int, bool) {
	v := os.Getenv(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// validateConfig 校验并规范化配置。
func validateConfig(cfg *Config) error {
	if cfg.APIBaseURL == "" {
		return errors.New("api_base_url is required (set via .env or API_BASE_URL env)")
	}
	u, err := url.Parse(cfg.APIBaseURL)
	if err != nil {
		return fmt.Errorf("invalid api_base_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("api_base_url scheme must be http or https, got %q", u.Scheme)
	}

	if cfg.DashboardUsername == "" {
		return errors.New("dashboard_username is required (set via .env or DASHBOARD_USERNAME env)")
	}
	if cfg.DashboardPassword == "" {
		return errors.New("dashboard_password is required (set via .env or DASHBOARD_PASSWORD env)")
	}
	if cfg.IntervalMinutes <= 0 {
		cfg.IntervalMinutes = 30
	}
	if cfg.RetryTimes < 0 {
		cfg.RetryTimes = 2
	}
	if cfg.RetryDelaySeconds < 0 {
		cfg.RetryDelaySeconds = 30
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 60
	}
	if cfg.AlertDebounceMinutes < 0 {
		cfg.AlertDebounceMinutes = 0
	}
	if cfg.LogRetentionDays < 0 {
		cfg.LogRetentionDays = 0
	}
	return nil
}
