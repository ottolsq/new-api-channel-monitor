package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const defaultConfigPath = "config.json"

func main() {
	var (
		configPath = flag.String("config", defaultConfigPath, "配置文件路径")
		queryHours = flag.Int("query", 0, "查询最近 N 小时的可用率，然后退出")
		failures   = flag.Bool("failures", false, "与 -query 配合，显示失败明细")
		once       = flag.Bool("once", false, "只运行一轮检测，然后退出")
	)
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	store, err := NewStorage(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	if *queryHours > 0 {
		if err := runQuery(store, *queryHours, *failures); err != nil {
			fmt.Fprintf(os.Stderr, "查询失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	auth, err := NewAuthManager(cfg.APIBaseURL, cfg.DashboardUsername, cfg.DashboardPassword)
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建认证管理器失败: %v\n", err)
		os.Exit(1)
	}

	client, err := NewAPIClient(cfg.APIBaseURL, auth, cfg.TimeoutSeconds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建 API 客户端失败: %v\n", err)
		os.Exit(1)
	}

	var alert *FeishuAlert
	if cfg.FeishuWebhookURL != "" {
		alert = NewFeishuAlert(cfg.FeishuWebhookURL, cfg.TimeoutSeconds)
	}

	monitor := NewMonitor(cfg, client, store, alert)

	if *once {
		if err := monitor.RunOnce(context.Background(), time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "运行失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	interval := time.Duration(cfg.IntervalMinutes) * time.Minute
	fmt.Printf("[启动] 渠道监控已启动，检测间隔 %s，配置 %s\n", interval, *configPath)
	if err := runLoop(monitor, interval); err != nil {
		fmt.Fprintf(os.Stderr, "运行失败: %v\n", err)
		os.Exit(1)
	}
}

// runLoop 按 interval 周期性地执行一轮检测，支持优雅退出。
func runLoop(monitor *Monitor, interval time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 启动立即执行一轮
	if err := monitor.RunOnce(ctx, time.Now()); err != nil {
		fmt.Printf("[运行错误] %v\n", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("[退出] 收到终止信号")
			return nil
		case now := <-ticker.C:
			if err := monitor.RunOnce(ctx, now); err != nil {
				fmt.Printf("[运行错误] %v\n", err)
			}
		}
	}
}

// runQuery 查询最近 hours 小时的可用率，可选展示失败明细。
func runQuery(store *Storage, hours int, showFailures bool) error {
	ctx := context.Background()
	now := time.Now()

	fmt.Printf("\n最近 %d 小时渠道模型可用率统计\n\n", hours)

	rows, err := store.QueryAvailability(ctx, hours, now)
	if err != nil {
		return err
	}

	fmt.Printf("%-8s | %-16s | %-24s | %-6s | %-6s | %-8s | %-10s\n",
		"渠道ID", "渠道名称", "模型", "总数", "成功", "可用率", "平均延迟")
	fmt.Println("---------+------------------+--------------------------+--------+--------+----------+------------")

	for _, r := range rows {
		color := "\x1b[31m"
		if r.AvailabilityPercent >= 99 {
			color = "\x1b[32m"
		} else if r.AvailabilityPercent >= 95 {
			color = "\x1b[33m"
		}
		fmt.Printf("%-8d | %-16s | %-24s | %-6d | %-6d | %s%7.2f%%\x1b[0m | %8.0fms\n",
			r.ChannelID, truncate(r.ChannelName, 16), truncate(r.Model, 24),
			r.Total, r.Success, color, r.AvailabilityPercent, r.AvgLatencyMs)
	}

	if showFailures {
		fmt.Println("\n失败明细：")
		failures, err := store.QueryRecentFailures(ctx, hours, now)
		if err != nil {
			return err
		}
		if len(failures) == 0 {
			fmt.Println("无失败记录")
			return nil
		}
		for _, f := range failures {
			fmt.Printf("[%s] %s (ID=%d) / %s: [%s] %s\n",
				f.CreatedAt.Format("2006-01-02 15:04:05"), f.ChannelName, f.ChannelID,
				f.Model, f.ErrorCode, truncate(f.ErrorMsg, 120))
		}
	}
	return nil
}

// truncate 按字符数截断字符串，避免截断多字节 UTF-8 字符。
func truncate(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-3]) + "..."
}
