package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
)

// Config 是监控工具的全局配置。
// 加载顺序：示例 config.json.example（兜底）→ --config 指定文件 → 环境变量覆盖。
type Config struct {
	APIBaseURL           string `json:"api_base_url"`
	DashboardUsername    string `json:"dashboard_username"`
	DashboardPassword    string `json:"dashboard_password"`
	FeishuWebhookURL     string `json:"feishu_webhook_url"`
	DBPath               string `json:"db_path"`
	IntervalMinutes      int    `json:"interval_minutes"`
	RetryTimes           int    `json:"retry_times"`
	RetryDelaySeconds    int    `json:"retry_delay_seconds"`
	TimeoutSeconds       int    `json:"timeout_seconds"`
	AlertDebounceMinutes int    `json:"alert_debounce_minutes"`
}

const (
	defaultExampleConfigPath = "config.json.example"
)

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

// loadConfig 加载配置：尝试 path（--config 指定），文件不存在则回退到 config.json.example；
// 然后用环境变量覆盖任何非空字段；最后校验必填项。
// 优先以"绝不报暴露的密码"为目标：env > 显式文件 > 默认值。
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()

	usedPath, data, err := readConfigFile(path)
	if err != nil {
		return cfg, err
	}
	if usedPath != "" && len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config (%s): %w", usedPath, err)
		}
	}

	applyEnvOverrides(&cfg)

	if err := validateConfig(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// readConfigFile 读取配置文件：先用 path，文件不存在则回退到 defaultExampleConfigPath。
// 返回 (实际使用路径, 内容, 错误)。
// 当 path 和 example 都缺失时返回空内容、不报错——交给 validateConfig 报错。
func readConfigFile(path string) (string, []byte, error) {
	if path == "" {
		path = defaultExampleConfigPath
	}

	if data, err := os.ReadFile(path); err == nil {
		return path, data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("read config %s: %w", path, err)
	}

	// 指定文件不存在 -> 回退到 example（如果存在）
	if _, statErr := os.Stat(defaultExampleConfigPath); statErr == nil {
		data, err := os.ReadFile(defaultExampleConfigPath)
		if err != nil {
			return "", nil, fmt.Errorf("read example config %s: %w", defaultExampleConfigPath, err)
		}
		return defaultExampleConfigPath, data, nil
	}

	// 都没有 -> 使用默认值，校验时会捕获必填项缺失
	return "", nil, nil
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
		return errors.New("api_base_url is required (set via config file or API_BASE_URL env)")
	}
	u, err := url.Parse(cfg.APIBaseURL)
	if err != nil {
		return fmt.Errorf("invalid api_base_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("api_base_url scheme must be http or https, got %q", u.Scheme)
	}

	if cfg.DashboardUsername == "" {
		return errors.New("dashboard_username is required (set via config file or DASHBOARD_USERNAME env)")
	}
	if cfg.DashboardPassword == "" {
		return errors.New("dashboard_password is required (set via config file or DASHBOARD_PASSWORD env)")
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
	return nil
}
