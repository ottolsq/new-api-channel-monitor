package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// FeishuAlert 飞书告警客户端。
type FeishuAlert struct {
	WebhookURL string
	Client     *http.Client
}

// NewFeishuAlert 创建飞书告警客户端。
// webhookURL 为空时 Send 直接返回 nil，不会触发网络请求。
func NewFeishuAlert(webhookURL string, timeoutSeconds int) *FeishuAlert {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	return &FeishuAlert{
		WebhookURL: webhookURL,
		Client:     &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second},
	}
}

// FailedModel 告警时用的失败模型信息。
type FailedModel struct {
	Model     string
	ErrorCode string
	ErrorMsg  string
}

// Send 发送飞书交互卡片告警。
func (f *FeishuAlert) Send(channelName string, failedModels []FailedModel) error {
	if f.WebhookURL == "" {
		return nil
	}

	modelLines := make([]string, 0, len(failedModels))
	for _, m := range failedModels {
		info := m.ErrorMsg
		if m.ErrorCode != "" {
			info = fmt.Sprintf("[%s] %s", m.ErrorCode, m.ErrorMsg)
		}
		modelLines = append(modelLines, fmt.Sprintf("• **%s** — %s", m.Model, info))
	}

	payload := map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"header": map[string]any{
				"title": map[string]any{
					"tag":     "plain_text",
					"content": "🔴 渠道模型异常告警",
				},
				"template": "red",
			},
			"elements": []map[string]any{
				{
					"tag": "div",
					"fields": []map[string]any{
						{
							"is_short": true,
							"text": map[string]any{
								"tag":     "lark_md",
								"content": fmt.Sprintf("**渠道名称**\n%s", channelName),
							},
						},
						{
							"is_short": true,
							"text": map[string]any{
								"tag":     "lark_md",
								"content": fmt.Sprintf("**检测时间**\n%s", time.Now().Format("2006-01-02 15:04:05")),
							},
						},
					},
				},
				{
					"tag": "div",
					"text": map[string]any{
						"tag":     "lark_md",
						"content": fmt.Sprintf("**异常模型 (%d 个)**\n%s", len(failedModels), strings.Join(modelLines, "\n")),
					},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal feishu payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, f.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create feishu request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.Client.Do(req)
	if err != nil {
		return fmt.Errorf("send feishu request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("feishu request failed with status %d", resp.StatusCode)
	}
	return nil
}
