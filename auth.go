package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// AuthManager 负责 new-api dashboard 登录并管理 access token。
// 它会缓存 access token，只在 token 即将过期时才重新登录，
// 避免高频检测触发 new-api 的登录限流。
type AuthManager struct {
	baseURL  *url.URL
	client   *http.Client
	username string
	password string

	mu          sync.RWMutex
	accessToken string
	expiresAt   time.Time
}

// NewAuthManager 创建认证管理器。
func NewAuthManager(baseURL, username, password string) (*AuthManager, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid api_base_url: %w", err)
	}
	u.Path = ""

	return &AuthManager{
		baseURL:  u,
		client:   &http.Client{Timeout: 30 * time.Second},
		username: username,
		password: password,
	}, nil
}

// GetAccessToken 调用 /api/user/login 拿一个新 token。
// 不缓存 token：父进程调度器每轮 fork 一个新子进程，子进程登录一次后立即退出，
// 进程结束 session 立即在 new-api 端进入可回收状态，避免长期会话堆积触发 AUTH_SESSION_LIMIT。
func (a *AuthManager) GetAccessToken(ctx context.Context) (string, error) {
	return a.loginOnce(ctx)
}

// loginOnce 单次登录，调用方拿到 token 后直接使用，进程退出后 token 随之失效。
func (a *AuthManager) loginOnce(ctx context.Context) (string, error) {
	token, _, err := a.login(ctx)
	return token, err
}

// login 调用 /api/user/login 获取 dashboard access token 及其过期时间。
func (a *AuthManager) login(ctx context.Context) (string, time.Time, error) {
	u := *a.baseURL
	u.Path = "/api/user/login"

	payload := map[string]string{
		"username": a.username,
		"password": a.password,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("marshal login payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("do login request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read login response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("login http %d: %s", resp.StatusCode, string(respBody))
	}

	var data struct {
		Success bool `json:"success"`
		Data    struct {
			AccessToken     string `json:"access_token"`
			AccessExpiresAt int64  `json:"access_expires_at"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return "", time.Time{}, fmt.Errorf("decode login response: %w", err)
	}

	if !data.Success {
		return "", time.Time{}, fmt.Errorf("login failed: %s", data.Message)
	}
	if data.Data.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("login response missing access_token")
	}

	expiresAt := time.Unix(data.Data.AccessExpiresAt, 0)
	// 如果返回的过期时间无效或已过期，保守使用 15 分钟
	if expiresAt.Before(time.Now().Add(time.Minute)) {
		expiresAt = time.Now().Add(15 * time.Minute)
	}

	return data.Data.AccessToken, expiresAt, nil
}
