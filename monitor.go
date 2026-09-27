package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Monitor 是调度器核心。
type Monitor struct {
	Config Config
	Client *APIClient
	Store  *Storage
	Alert  *FeishuAlert
}

// NewMonitor 创建监控实例。
func NewMonitor(cfg Config, client *APIClient, store *Storage, alert *FeishuAlert) *Monitor {
	return &Monitor{
		Config: cfg,
		Client: client,
		Store:  store,
		Alert:  alert,
	}
}

// RunOnce 执行一轮检测：拉取所有启用渠道，测试每个模型。
func (m *Monitor) RunOnce(ctx context.Context, now time.Time) error {
	channels, err := m.Client.ListChannels(ctx)
	if err != nil {
		return fmt.Errorf("list channels: %w", err)
	}

	enabledChannels := filterEnabledChannels(channels)
	if len(enabledChannels) == 0 {
		fmt.Println("[跳过] 无启用渠道")
		return nil
	}

	fmt.Printf("[开始] 共 %d 个启用渠道\n", len(enabledChannels))
	for _, ch := range enabledChannels {
		if err := ctx.Err(); err != nil {
			fmt.Println("[取消] 收到上下文取消信号")
			return err
		}
		if err := m.testChannel(ctx, ch, now); err != nil {
			fmt.Printf("[错误] 渠道 %s (ID=%d) 测试失败: %v\n", ch.Name, ch.ID, err)
		}
	}
	fmt.Println("[完成] 全部测试完成")
	return nil
}

// filterEnabledChannels 过滤出 status = 1 的启用渠道。
// new-api 渠道状态定义：1 启用，2 手动停用，3 自动停用。
func filterEnabledChannels(channels []Channel) []Channel {
	enabled := make([]Channel, 0, len(channels))
	for _, ch := range channels {
		if ch.Status == 1 {
			enabled = append(enabled, ch)
		}
	}
	return enabled
}

// testChannel 测试单个渠道的全部模型。
func (m *Monitor) testChannel(ctx context.Context, ch Channel, now time.Time) error {
	fmt.Printf("\n[开始] 渠道 %s (ID=%d)\n", ch.Name, ch.ID)
	models := splitModels(ch.Models)
	if len(models) == 0 {
		fmt.Printf("[跳过] 渠道 %s 无模型\n", ch.Name)
		return nil
	}

	fmt.Printf("[信息] 渠道 %s 共 %d 个模型\n", ch.Name, len(models))

	failedModels := make([]FailedModel, 0, len(models))
	failedLogIDs := make(map[int]FailedModel)
	successCount := 0
	totalLatency := 0

	for _, model := range models {
		if err := ctx.Err(); err != nil {
			fmt.Printf("[取消] 模型 %s 测试中断\n", model)
			return err
		}
		fmt.Printf("[测试] %s...\n", model)
		result := m.testModelWithRetry(ctx, ch.ID, model)

		logNow := time.Now()
		logID, err := m.Store.InsertLog(ctx, ch.ID, ch.Name, model, result, logNow)
		if err != nil {
			fmt.Printf("  [落库失败] %s: %v\n", model, err)
		}

		if result.Success {
			successCount++
			totalLatency += result.LatencyMs
			fmt.Printf("  [成功] %dms\n", result.LatencyMs)
			continue
		}

		// 视频模型不支持测试、未配置价格等预期内错误，只记录不告警
		if isSkippableError(result.ErrorCode, result.ErrorMsg) {
			fmt.Printf("  [跳过] %s: [%s] %s\n", model, result.ErrorCode, result.ErrorMsg)
			continue
		}

		fmt.Printf("  [失败] [%s] %s\n", result.ErrorCode, result.ErrorMsg)
		fm := FailedModel{
			Model:     model,
			ErrorCode: result.ErrorCode,
			ErrorMsg:  result.ErrorMsg,
		}
		failedModels = append(failedModels, fm)
		if logID > 0 {
			failedLogIDs[int(logID)] = fm
		}
	}

	avgLatency := 0
	if successCount > 0 {
		avgLatency = totalLatency / successCount
	}
	fmt.Printf("[完成] 渠道 %s: %d/%d 成功, %d 跳过, 平均延迟 %dms\n", ch.Name, successCount, len(models), len(models)-successCount-len(failedModels), avgLatency)

	if len(failedModels) > 0 {
		if err := m.alertIfNeeded(ctx, ch, failedModels, failedLogIDs, now); err != nil {
			fmt.Printf("[告警错误] %s: %v\n", ch.Name, err)
		}
	}

	return nil
}

// isSkippableError 判断是否为预期内、不需要告警的错误。
// new-api 测试接口对部分场景不返回 error_code，所以以 message 文本特征为主判断。
//
// 三类预期内错误：
//  1. 不支持测试的渠道类型（视频模型等）—— message 形如 "Kling channel test is not supported"
//     源码：controller/channel-test.go:93
//  2. 模型未配置价格 —— message 含 "价格未配置"/"价格尚未由管理员配置"/"price not configured"
//     源码：relay/helper/price.go:21-34 (modelPriceNotConfiguredError)
//  3. 上游限流 429 —— 视频等模型挂在 VolcEngine 等可测试渠道下时，chat 探针打到
//     上游账号级模型 RPM 限制（如火山方舟 ModelAccountRpmRateLimitExceeded），属确定性失败
func isSkippableError(code, message string) bool {
	switch code {
	case "model_price_error":
		return true
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "channel test is not supported"):
		return true
	case strings.Contains(message, "价格未配置"):
		return true
	case strings.Contains(message, "价格尚未由管理员配置"):
		return true
	case strings.Contains(lower, "price not configured"):
		return true
	case strings.Contains(lower, "does not support this api"):
		return true
	case strings.Contains(lower, "has not been priced by the administrator"):
		return true
	case strings.Contains(lower, "status code 429"),
		strings.Contains(lower, "rate limit"),
		strings.Contains(lower, "rate_limit"),
		strings.Contains(lower, "too many requests"),
		strings.Contains(lower, "ratelimitexceeded"):
		return true
	}
	return false
}

// testModelWithRetry 对单个模型执行测试，失败时按配置重试。
// 总尝试次数 = RetryTimes + 1，即“重试 RetryTimes 次，全部失败则确认异常”。
func (m *Monitor) testModelWithRetry(ctx context.Context, channelID int, model string) TestResult {
	var lastResult TestResult
	maxAttempts := m.Config.RetryTimes + 1

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			fmt.Printf("  [重试 %d/%d] %s，等待 %ds...\n", attempt, m.Config.RetryTimes, model, m.Config.RetryDelaySeconds)
			if err := sleepWithContext(ctx, time.Duration(m.Config.RetryDelaySeconds)*time.Second); err != nil {
				return TestResult{
					Success:   false,
					ErrorCode: "canceled",
					ErrorMsg:  "等待重试期间被取消",
				}
			}
		}

		lastResult = m.Client.TestChannelModel(ctx, channelID, model, m.Config.TimeoutSeconds)
		if lastResult.Success {
			return lastResult
		}
		fmt.Printf("  [失败 %d/%d] %s: [%s] %s\n", attempt+1, maxAttempts, model, lastResult.ErrorCode, lastResult.ErrorMsg)

		// 预期内错误（视频模型不支持测试、模型未配置价格）是确定性失败，
		// 重试不可能改变结果，直接返回避免空等 RetryDelaySeconds * RetryTimes。
		if isSkippableError(lastResult.ErrorCode, lastResult.ErrorMsg) {
			return lastResult
		}
	}

	return lastResult
}

// sleepWithContext 在上下文取消时可中断的 sleep。
func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// alertIfNeeded 对需要去抖的模型发送飞书告警，并精确标记对应日志已告警。
func (m *Monitor) alertIfNeeded(ctx context.Context, ch Channel, failedModels []FailedModel, failedLogIDs map[int]FailedModel, now time.Time) error {
	if m.Alert == nil || m.Config.FeishuWebhookURL == "" {
		return nil
	}

	needAlertModels := make([]FailedModel, 0, len(failedModels))
	needAlertLogIDs := make([]int64, 0, len(failedModels))

	for _, fm := range failedModels {
		should, err := m.Store.ShouldAlert(ctx, ch.ID, fm.Model, m.Config.AlertDebounceMinutes, now)
		if err != nil {
			fmt.Printf("  [告警判断失败] %s/%s: %v\n", ch.Name, fm.Model, err)
			continue
		}
		if should {
			needAlertModels = append(needAlertModels, fm)
			for id, mfm := range failedLogIDs {
				if mfm.Model == fm.Model {
					needAlertLogIDs = append(needAlertLogIDs, int64(id))
					break
				}
			}
		} else {
			fmt.Printf("  [告警防抖] %s/%s 跳过\n", ch.Name, fm.Model)
		}
	}

	if len(needAlertModels) == 0 {
		return nil
	}

	if err := m.Alert.Send(ch.Name, needAlertModels); err != nil {
		return err
	}
	fmt.Printf("[飞书] 告警已发送: %s - %d 个模型异常\n", ch.Name, len(needAlertModels))

	for _, logID := range needAlertLogIDs {
		_ = m.Store.MarkAlertedByID(ctx, logID)
	}
	return nil
}
