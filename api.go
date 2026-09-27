package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// APIClient 封装对 new-api 的 HTTP 调用。
// 该客户端不依赖 new-api 的任何内部包，仅通过公开 API 交互。
type APIClient struct {
	baseURL        *url.URL      // 解析后的基础 URL，避免字符串拼接导致路径异常
	client         *http.Client  // 共享 HTTP 客户端（只读，不会被修改）
	defaultTimeout time.Duration // 默认请求超时
	auth           *AuthManager  // 负责获取 dashboard access token
}

// NewAPIClient 创建一个 new-api 客户端。
// baseURL 必须是 http:// 或 https:// 开头的合法 URL。
func NewAPIClient(baseURL string, auth *AuthManager, timeoutSeconds int) (*APIClient, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 60
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid api_base_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("api_base_url scheme must be http or https, got %q", u.Scheme)
	}
	// 去掉尾部斜杠，统一拼接行为
	u.Path = "" // 基础 URL 不应包含路径，所有路径通过 request 参数传入

	return &APIClient{
		baseURL:        u,
		client:         &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second},
		defaultTimeout: time.Duration(timeoutSeconds) * time.Second,
		auth:           auth,
	}, nil
}

// request 发送 HTTP 请求并解析 JSON 响应。
// path 必须是 / 开头的绝对路径；query 可选。
func (c *APIClient) request(ctx context.Context, method, path string, query url.Values) (map[string]any, error) {
	if !hasLeadingSlash(path) {
		return nil, fmt.Errorf("request path must start with /: %q", path)
	}

	accessToken, err := c.auth.GetAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	// 复制基础 URL，避免并发修改共享对象
	u := *c.baseURL
	u.Path = path
	if query != nil {
		u.RawQuery = query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("decode response (%d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return data, fmt.Errorf("%w: http %d: %s", errUnauthorized, resp.StatusCode, string(body))
	}
	if resp.StatusCode != http.StatusOK {
		return data, fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}
	return data, nil
}

// errUnauthorized 用于上层识别 access token 失效。
var errUnauthorized = fmt.Errorf("unauthorized")

// IsUnauthorized 判断错误是否为 401 未授权。
func IsUnauthorized(err error) bool {
	return errors.Is(err, errUnauthorized)
}

// hasLeadingSlash 检查字符串是否以 / 开头。
func hasLeadingSlash(s string) bool {
	return len(s) > 0 && s[0] == '/'
}

// Channel 表示 new-api 返回的渠道结构。
type Channel struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Type   int    `json:"type"`
	Status int    `json:"status"`
	Models string `json:"models"`
}

// ListChannels 调用 GET /api/channel/ 获取全部渠道列表，支持分页。
// new-api 返回格式为 { success: true, data: { items: [...], total, page, page_size } }
func (c *APIClient) ListChannels(ctx context.Context) ([]Channel, error) {
	allChannels := make([]Channel, 0, 64)
	page := 1
	pageSize := 100 // 上限，避免过多请求

	for {
		resp, err := c.request(ctx, http.MethodGet, "/api/channel/", url.Values{
			"p":         {strconv.Itoa(page)},
			"page_size": {strconv.Itoa(pageSize)},
		})
		if err != nil {
			return nil, fmt.Errorf("list channels page %d: %w", page, err)
		}

		success, _ := resp["success"].(bool)
		if !success {
			return nil, fmt.Errorf("list channels page %d failed: %v", page, resp["message"])
		}

		data, ok := resp["data"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unexpected list channels response format: missing data object")
		}

		items, ok := data["items"].([]any)
		if !ok {
			return nil, fmt.Errorf("unexpected list channels response format: missing items array")
		}

		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			ch := channelFromMap(m)
			if ch.ID == 0 || ch.Name == "" {
				continue
			}
			allChannels = append(allChannels, ch)
		}

		// 判断是否已拉完
		totalVal, _ := toFloat64(data["total"])
		total := int(totalVal)
		if total == 0 {
			break
		}
		if len(allChannels) >= total {
			break
		}
		// 本页不足一页（刚好是最后一页）或已拿到全部，退出
		if len(items) < pageSize {
			break
		}
		page++
	}

	return allChannels, nil
}

// channelFromMap 将 map 解析为 Channel，对缺失字段保持零值。
func channelFromMap(m map[string]any) Channel {
	ch := Channel{}
	if v, ok := toFloat64(m["id"]); ok {
		ch.ID = int(v)
	}
	if v, ok := m["name"].(string); ok {
		ch.Name = v
	}
	if v, ok := toFloat64(m["type"]); ok {
		ch.Type = int(v)
	}
	if v, ok := toFloat64(m["status"]); ok {
		ch.Status = int(v)
	}
	if v, ok := m["models"].(string); ok {
		ch.Models = v
	}
	return ch
}

// toFloat64 将常见数值类型转为 float64。
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// TestResult 表示单模型测试结果。
type TestResult struct {
	Success   bool
	LatencyMs int
	ErrorCode string
	ErrorMsg  string
}

// TestChannelModel 调用 GET /api/channel/test/:id?model=xxx 测试单个模型。
// timeoutSeconds <= 0 时使用客户端默认超时。
func (c *APIClient) TestChannelModel(ctx context.Context, channelID int, modelName string, timeoutSeconds int) TestResult {
	timeout := c.defaultTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}

	// 每次请求创建独立的超时上下文，不修改共享 http.Client
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	q := url.Values{}
	if modelName != "" {
		q.Set("model", modelName)
	}

	resp, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/api/channel/test/%d", channelID), q)
	if err != nil {
		return TestResult{
			Success:   false,
			LatencyMs: 0,
			ErrorCode: "request_failed",
			ErrorMsg:  err.Error(),
		}
	}

	success, _ := resp["success"].(bool)
	if success {
		timeVal, _ := toFloat64(resp["time"])
		return TestResult{
			Success:   true,
			LatencyMs: int(timeVal * 1000),
		}
	}

	timeVal, _ := toFloat64(resp["time"])
	code, _ := resp["error_code"].(string)
	msg, _ := resp["message"].(string)
	if code == "" {
		code = "unknown"
	}
	if msg == "" {
		msg = "渠道测试返回失败"
	}
	return TestResult{
		Success:   false,
		LatencyMs: int(timeVal * 1000),
		ErrorCode: code,
		ErrorMsg:  msg,
	}
}
