package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// childModeFlag 是隐藏标志：当进程以 `-child` 启动时，进入单轮子进程模式，
// 跑完 RunOnce 后立即退出；不带此标志的进程是父进程，负责周期性 fork + 杀子进程。
const childModeFlag = "-child"

// runAsChild 是子进程入口：跑一轮检测后退出，进程退出码映射到错误。
// 父进程通过退出码判断本次检测是否成功。
func runAsChild() int {
	cfg, err := loadConfig()
	if err != nil {
		log.Printf("[子进程] 加载配置失败: %v\n", err)
		return 2
	}

	store, err := NewStorage(cfg.DBPath)
	if err != nil {
		log.Printf("[子进程] 初始化数据库失败: %v\n", err)
		return 2
	}
	defer store.Close()

	auth, err := NewAuthManager(cfg.APIBaseURL, cfg.DashboardUsername, cfg.DashboardPassword)
	if err != nil {
		log.Printf("[子进程] 创建认证管理器失败: %v\n", err)
		return 2
	}

	client, err := NewAPIClient(cfg.APIBaseURL, auth, cfg.TimeoutSeconds)
	if err != nil {
		log.Printf("[子进程] 创建 API 客户端失败: %v\n", err)
		return 2
	}

	var alert *FeishuAlert
	if cfg.FeishuWebhookURL != "" {
		alert = NewFeishuAlert(cfg.FeishuWebhookURL, cfg.TimeoutSeconds)
	}

	monitor := NewMonitor(cfg, client, store, alert)

	if err := monitor.RunOnce(context.Background(), time.Now()); err != nil {
		log.Printf("[子进程] 检测失败: %v\n", err)
		return 1
	}
	return 0
}

// Supervisor 是父进程调度器：每隔 interval 启动一个全新的子进程跑一轮检测。
// 每轮都是独立进程，access_token 不缓存，登录会话立即释放，
// 避免 long-lived session 堆积触发 new-api 的 AUTH_SESSION_LIMIT。
type Supervisor struct {
	interval      time.Duration
	exe           string
	childGraceful time.Duration // 给子进程优雅退出的宽限期，超时后 SIGKILL
}

// NewSupervisor 创建一个调度器。
func NewSupervisor(interval time.Duration) (*Supervisor, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate executable: %w", err)
	}
	return &Supervisor{
		interval:      interval,
		exe:           exe,
		childGraceful: 10 * time.Second,
	}, nil
}

// Run 启动调度循环：ticker 到点 → fork 子进程 → 等待退出 → 进入下一轮。
// 收到 SIGINT/SIGTERM 后等当前子进程结束后再退出。
func (s *Supervisor) Run(ctx context.Context) error {
	log.Printf("[启动] 父进程调度已启动，检测间隔 %s\n", s.interval)

	// 启动立刻跑一轮，避免冷启动还要等一个完整周期
	if !s.spawnAndWait(ctx) {
		return nil // 收到终止信号
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[退出] 收到终止信号")
			return nil
		case <-ticker.C:
			if !s.spawnAndWait(ctx) {
				return nil
			}
		}
	}
}

// spawnAndWait fork 一个子进程跑一轮检测，等待其退出。
// 返回 false 表示父进程收到了终止信号，需要退出调度循环。
func (s *Supervisor) spawnAndWait(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, s.exe, childModeFlag)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	// 子进程与父进程脱离独立进程组，方便单独 kill
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		log.Printf("[调度错误] 启动子进程失败: %v\n", err)
		return true
	}
	pid := cmd.Process.Pid
	log.Printf("[调度] 子进程 PID=%d 已启动\n", pid)

	// 用一个独立 channel 监听退出，避免阻塞在 cmd.Wait 上错过 ctx 取消
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		// 父进程要退出：先把子进程干掉再返回
		log.Printf("[调度] 父进程收到终止信号，向子进程 PID=%d 发送 SIGTERM\n", pid)
		s.terminateChild(cmd, pid)
		<-done
		return false
	case err := <-done:
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				log.Printf("[调度] 子进程 PID=%d 退出码=%d\n", pid, exitErr.ExitCode())
			} else {
				log.Printf("[调度] 子进程 PID=%d 异常退出: %v\n", pid, err)
			}
		} else {
			log.Printf("[调度] 子进程 PID=%d 已正常退出\n", pid)
		}
		return true
	}
}

// terminateChild 给子进程发 SIGTERM，宽限期内未退出则 SIGKILL。
// 子进程独立进程组，所以发信号到 PID 即可，不需要负 PID。
func (s *Supervisor) terminateChild(cmd *exec.Cmd, pid int) {
	if cmd.Process == nil {
		return
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		log.Printf("[调度] 向子进程 PID=%d 发 SIGTERM 失败: %v\n", pid, err)
		return
	}
	timer := time.NewTimer(s.childGraceful)
	defer timer.Stop()
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-timer.C:
		log.Printf("[调度] 子进程 PID=%d 未在 %s 内退出，强制 SIGKILL\n", pid, s.childGraceful)
		_ = cmd.Process.Kill()
		<-done
	}
}
