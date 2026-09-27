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

// GetAccessToken 返回可用于调用 /api/* 管理接口的 dashboard access token。
// 如果缓存的 token 仍有效，则直接返回；否则重新登录获取。
func (a *AuthManager) GetAccessToken(ctx context.Context) (string, error) {
	// 先尝试读取缓存，token 剩余有效期大于 1 分钟认为可用
	a.mu.RLock()
	token := a.accessToken
	expiresAt := a.expiresAt
	a.mu.RUnlock()

	if token != "" && time.Until(expiresAt) > time.Minute {
		return token, nil
	}

	// 缓存无效，重新登录
	a.mu.Lock()
	defer a.mu.Unlock()

	// 双重检查，避免多个 goroutine 同时登录
	if a.accessToken != "" && time.Until(a.expiresAt) > time.Minute {
		return a.accessToken, nil
	}

	token, expiresAt, err := a.login(ctx)
	if err != nil {
		return "", err
	}

	a.accessToken = token
	a.expiresAt = expiresAt
	return token, nil
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
