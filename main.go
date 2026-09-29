package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const defaultDBPathHint = "monitor.db"

func main() {
	// `-child` 是父进程调度器传给自己的隐藏标志，不对外暴露；识别到就走单轮子进程模式。
	for _, a := range os.Args[1:] {
		if a == childModeFlag {
			os.Exit(runAsChild())
		}
	}

	var (
		queryHours = flag.Int("query", 0, "查询最近 N 小时的可用率，然后退出")
		failures   = flag.Bool("failures", false, "与 -query 配合，显示失败明细")
		once       = flag.Bool("once", false, "只运行一轮检测，然后退出")
	)
	flag.Parse()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	store, err := NewStorage(cfg.DBPath)
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	defer store.Close()

	if *queryHours > 0 {
		if err := runQuery(store, *queryHours, *failures); err != nil {
			log.Fatalf("查询失败: %v", err)
		}
		return
	}

	if *once {
		client, alert, err := buildClientAndAlert(cfg)
		if err != nil {
			log.Fatalf("%v", err)
		}
		monitor := NewMonitor(cfg, client, store, alert)
		if err := monitor.RunOnce(context.Background(), time.Now()); err != nil {
			log.Fatalf("运行失败: %v", err)
		}
		return
	}

	// 默认模式：父进程周期性 fork 子进程跑检测，每轮独立进程不缓存 session。
	interval := time.Duration(cfg.IntervalMinutes) * time.Minute
	retention := "永久"
	if cfg.LogRetentionDays > 0 {
		retention = fmt.Sprintf("%d 天", cfg.LogRetentionDays)
	}
	log.Printf("[启动] 父进程调度模式，检测间隔 %s，日志保留 %s\n", interval, retention)

	sup, err := NewSupervisor(interval)
	if err != nil {
		log.Fatalf("创建调度器失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := sup.Run(ctx); err != nil {
		log.Fatalf("运行失败: %v", err)
	}
}

// buildClientAndAlert 给 -once / 手动调试用，构造 APIClient 与可选的飞书告警。
func buildClientAndAlert(cfg Config) (*APIClient, *FeishuAlert, error) {
	auth, err := NewAuthManager(cfg.APIBaseURL, cfg.DashboardUsername, cfg.DashboardPassword)
	if err != nil {
		return nil, nil, fmt.Errorf("创建认证管理器失败: %v", err)
	}
	client, err := NewAPIClient(cfg.APIBaseURL, auth, cfg.TimeoutSeconds)
	if err != nil {
		return nil, nil, fmt.Errorf("创建 API 客户端失败: %v", err)
	}
	var alert *FeishuAlert
	if cfg.FeishuWebhookURL != "" {
		alert = NewFeishuAlert(cfg.FeishuWebhookURL, cfg.TimeoutSeconds)
	}
	return client, alert, nil
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
