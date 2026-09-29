# channel-monitor

> 零入侵式 new-api 渠道可用性监控：自动拉取启用渠道、逐模型健康检测、失败重试、飞书告警、SQLite 留痕。

[![Go Version](https://img.shields.io/badge/go-1.22+-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](./LICENSE)
[![Docker Image](https://img.shields.io/badge/docker-%3Cdocker--hub--user%3E%2Fnew--api--channel--monitor-blue)](https://hub.docker.com/r/<docker-hub-user>/new-api-channel-monitor)

## 它在做什么

`channel-monitor` 周期性地检测 [new-api](https://github.com/QuantumNous/new-api) 各渠道下模型是否还能正常响应，自动完成：

1. 用 dashboard 账号登录 new-api 拿 `access_token`（**默认模式由父进程每轮 fork 子进程跑一次**，登录一次即退出，session 立即释放）
2. 分页拉取所有启用渠道（`status=1`）
3. 逐模型调用 `GET /api/channel/test/:id?model=xxx`
4. 失败按配置重试；预期内错误（视频模型不支持测试、价格未配置、上游 429）自动跳过不告警
5. 检测结果入库 SQLite，可按时间段查可用率

适合需要「渠道挂了我第一时间知道」又不希望侵入业务数据库或改 new-api 源码的团队。

## 特性

- **零入侵**：仅调用 new-api 公开 HTTP API，不动业务代码与数据库
- **进程级隔离检测**：默认模式由父进程按 `INTERVAL_MINUTES` 周期 fork 子进程跑一轮检测，**子进程退出即释放登录会话**，从源头避免 long-lived session 堆积触发 new-api 的 `AUTH_SESSION_LIMIT`（HTTP 409）。`-once` 保留用于手动单轮调试
- **日志带时间戳**：每行运行日志前缀执行时间（`2026/09/29 14:30:05 [开始] ...`）
- **日志保留期**：可配置自动清理 N 天前的检测记录（如 `LOG_RETENTION_DAYS=7` 或 `14`），默认 0 永久保留
- **自动拉取**：登录后调用 `GET /api/channel/?p=N&page_size=100` 翻页拉全量渠道
- **跳过停用渠道**：`status=1` 才检测，2（手动停用）/ 3（自动停用）自动跳过
- **失败重试**：失败重试 N 次，间隔可配
- **预期内错误不告警**：
  - message 含 `channel test is not supported` → 视频模型等跳过
  - message 含 `价格未配置` / `price not configured` → 未配置价格跳过
  - message 含 `status code 429` / `rate limit` / `too many requests` → 上游限流跳过
- **飞书告警**：重试仍失败的渠道模型推送 Lark 交互卡片，红色模板
- **告警防抖**：同一渠道同一模型在窗口内只告警 1 次
- **可用率查询**：`channel-monitor -query 24` 直接看最近 N 小时统计
- **优雅退出**：响应 `SIGINT` / `SIGTERM`，重试等待期间可立即取消

## 安装

### 方式一：下载预编译二进制

从 [Releases](../../releases) 页面挑选对应平台的压缩包，解压即用：

```bash
# 例如 Linux amd64
tar -xzf channel-monitor_linux_amd64.tar.gz
./channel-monitor --help
```

### 方式二：拉 Docker 镜像

```bash
docker pull <docker-hub-user>/new-api-channel-monitor:latest
```

### 方式三：从源码编译

```bash
git clone https://github.com/<your-org>/new-api-channel-monitor.git
cd new-api-channel-monitor
go build -o channel-monitor .
```

## 快速开始

### 1. 准备配置

```bash
cp .env.example .env
# 用编辑器改 .env，至少填 DASHBOARD_USERNAME / DASHBOARD_PASSWORD
# 程序启动时自动读取工作目录下的 .env
```

### 2. 跑一轮试试

```bash
./channel-monitor -once
```

### 3. 上常驻

```bash
./channel-monitor          # 父进程调度模式：每 INTERVAL_MINUTES fork 一个子进程跑一轮，进程退出即释放 session
```

#### 调度模式工作原理

默认模式由父进程调度：每隔 `INTERVAL_MINUTES` fork 一个**全新的子进程**执行 `RunOnce`，子进程退出后立即在 new-api 端释放登录会话。这样无论运行多久都只有「瞬时活跃 session」，不会触发 `USER_SESSION_ACTIVE_LIMIT` 触顶。

```
┌─────────────┐   fork   ┌─────────────┐
│ 父进程       │ ───────► │ 子进程 PID=N  │
│ (调度/ticker)│          │ RunOnce + Exit│
└─────────────┘          └─────────────┘
        │                       │
        │  SIGTERM 子进程        │ 进程退出 → session 立即在 new-api 进入可回收
        ▼                       ▼
   优雅停止             exit code: 0=成功 / 1=检测失败 / 2=初始化失败
```

- 父进程收 `SIGINT`/`SIGTERM` → 给子进程发 `SIGTERM` → 10s 宽限期 → `SIGKILL`
- `-child` 是隐藏标志，仅父进程 fork 时使用，**不要手动执行**

### 4. 查历史可用率

```bash
./channel-monitor -query 24             # 最近 24 小时可用率统计
./channel-monitor -query 24 -failures   # 同时列出失败明细
```

## 配置

### 配置来源与优先级

```
进程环境变量（export / docker -e）  →  工作目录 .env  →  代码内默认值
```

程序启动时自动加载工作目录下的 `.env`（`KEY=VALUE`，`#` 注释），其中未在进程环境中设置的变量会被注入——即 `export` 或 `docker -e` 的值可以覆盖 `.env`。典型用法：`.env` 写账号密码等敏感信息，容器编排里用环境变量微调。

### 配置项

| 环境变量 | 说明 | 默认值 |
|----------|------|--------|
| `API_BASE_URL` | new-api 地址（仅允许 http:// 或 https://） | `http://localhost:3000` |
| `DASHBOARD_USERNAME` | new-api 后台**管理员**用户名（必须 `role≥50` 才能调 `/api/channel/`） | 必填 |
| `DASHBOARD_PASSWORD` | new-api 后台管理员密码 | 必填 |
| `FEISHU_WEBHOOK_URL` | 飞书机器人 Webhook；留空则不告警 | 可选 |
| `DB_PATH` | SQLite 数据库路径 | 可执行文件同目录 `monitor.db` |
| `INTERVAL_MINUTES` | 一轮完整检测的间隔分钟数 | 30 |
| `RETRY_TIMES` | 模型测试失败后的重试次数 | 2 |
| `RETRY_DELAY_SECONDS` | 重试之间的等待秒数 | 30 |
| `TIMEOUT_SECONDS` | 单模型测试超时秒数 | 60 |
| `ALERT_DEBOUNCE_MINUTES` | 同渠道同模型告警防抖窗口；0 表示不防抖 | 60 |
| `LOG_RETENTION_DAYS` | 检测日志保留天数，每轮检测前自动清理更早的记录；0 表示永久保留 | 0 |

`INTERVAL_MINUTES` 是唯一的检测周期配置。程序每隔这么久跑一轮覆盖所有启用渠道的所有模型。

## 运行

### 命令一览

```bash
channel-monitor                  # 父进程调度模式：周期 fork 子进程跑检测（默认）
channel-monitor -once            # 单进程跑一轮，然后退出（手动调试用）
channel-monitor -query 24        # 最近 24 小时可用率
channel-monitor -query 24 -failures  # 同时列出失败明细
```

### Docker

**docker-compose（推荐）：**

```bash
# 在项目目录下创建 .env 后
docker compose up -d
```

容器内查询：

```bash
docker exec new-api-channel-monitor /app/channel-monitor -query 24 -failures
```

**纯 docker run：**

```bash
docker run -d --name new-api-channel-monitor \
  --env-file .env \
  -e DB_PATH=/app/data/monitor.db \
  -v channel-monitor-data:/app/data \
  new-api-channel-monitor:latest
```

### systemd

`/etc/systemd/system/channel-monitor.service`：

```ini
[Unit]
Description=new-api channel monitor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/opt/new-api-channel-monitor/.env
ExecStart=/opt/new-api-channel-monitor/channel-monitor
Restart=always
WorkingDirectory=/opt/new-api-channel-monitor

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now channel-monitor.service
```

### Windows 任务计划程序

```powershell
$action = New-ScheduledTaskAction -Execute "C:\path\to\channel-monitor.exe"
$trigger = New-ScheduledTaskTrigger -Once -At (Get-Date) -RepetitionInterval (New-TimeSpan -Minutes 30)
Register-ScheduledTask -Action $action -Trigger $trigger -TaskName "NewAPI-ChannelMonitor" -User "SYSTEM"
```

## 数据库表结构

```sql
CREATE TABLE test_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id INTEGER NOT NULL,
    channel_name TEXT NOT NULL,
    model TEXT NOT NULL,
    success INTEGER NOT NULL,
    latency_ms INTEGER,
    error_code TEXT,
    error_msg TEXT,
    alerted_at INTEGER DEFAULT 0,
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_test_logs_channel_model_created
  ON test_logs(channel_id, model, created_at DESC);
CREATE INDEX idx_test_logs_created_at
  ON test_logs(created_at);
CREATE INDEX idx_test_logs_alert
  ON test_logs(channel_id, model, success, alerted_at, created_at);
```

可用率查询就是 `WHERE created_at >= now - N hours` 分组聚合。

## 开发

### 目录结构

```
channel-monitor/
├── main.go              # 入口、命令行解析、查询命令
├── scheduler.go         # 父进程调度器：周期 fork 子进程跑检测
├── config.go            # 配置结构、.env 加载、环境变量覆盖、默认值
├── auth.go              # new-api dashboard 登录（不缓存 token）
├── api.go               # new-api HTTP 客户端（含分页拉全量渠道）
├── monitor.go           # 检测调度、重试、告警决策（预期内错误判断）
├── feishu.go            # 飞书交互卡片构造与发送
├── storage.go           # SQLite 表结构、可用率查询、告警防抖、日志清理
├── .env.example         # 环境变量配置模板
├── Dockerfile           # 多阶段构建
├── docker-compose.yml   # 独立参考编排
├── go.mod / go.sum      # Go 模块
└── README.md
```

### 关键约定

- 预期内错误以 `ErrorMsg` 文本特征匹配；具体匹配规则见 `monitor.go::isSkippableError` 的注释
- `status = 1` 才是 new-api 转发时使用的「启用」标志
- 默认模式由父进程调度，每轮 fork 新子进程跑检测；进程退出即释放 new-api 端登录会话，避免 long-lived session 触发 `AUTH_SESSION_LIMIT`
- `DASHBOARD_USERNAME` 必须是管理员角色（`role≥50`），普通用户调 `/api/channel/` 会被 409 拒绝

### 本地编译

```bash
go build ./...
gofmt -l .    # 应无输出
go vet ./...  # 应无警告
```

## 常见问题

**Q：登录频繁触发限流怎么办？**
A：默认模式是父进程 fork 子进程跑检测，每轮都是独立进程，**登录一次就退出**，不会累积会话；`-once` 手动调试时注意别连按。如仍触发，调大 `INTERVAL_MINUTES` 即可，建议 30 分钟以上。

**Q：出现 `AUTH_SESSION_LIMIT` (HTTP 409) 怎么排查？**
A：先确认 `DASHBOARD_USERNAME` 对应账号在 new-api 是**管理员角色**（`role=50` Admin 或 `role=100` Super Admin），`role=10` Common User 调 `/api/channel/` 会被拒绝并复用这个错误码。在数据库里：

```sql
UPDATE users SET role = 100 WHERE username = 'channel-monitor';
```

如果账号确实有权限但仍然 409，说明 new-api 端该账号的活跃 session 数已达 `USER_SESSION_ACTIVE_LIMIT`（默认 50）。手动撤销旧 session：

```sql
UPDATE user_sessions SET revoked_at = EXTRACT(EPOCH FROM NOW())
WHERE user_id = (SELECT id FROM users WHERE username = 'channel-monitor')
  AND revoked_at IS NULL
  AND id NOT IN (
    SELECT id FROM user_sessions
    WHERE user_id = (SELECT id FROM users WHERE username = 'channel-monitor')
      AND revoked_at IS NULL
    ORDER BY last_active_at DESC LIMIT 3
  );
```

或在 new-api 启动环境变量里调大 `USER_SESSION_ACTIVE_LIMIT`。

**Q：为什么默认用父进程 fork 子进程而不是常驻 goroutine？**
A：常驻进程会在 new-api 端维持一个长生命周期 session；监控账号一旦登录后 token 默认 15~30 分钟有效，每次续签还会新建 session。即使加了缓存，时间一长仍会触发 `USER_SESSION_ACTIVE_LIMIT`（默认 50）。fork 子进程模式每轮都是独立进程，进程退出即释放会话，从源头避免这个问题。

**Q：`GET /api/channel/` 一页只有 10 条，是丢数据了吗？**
A：默认分页就是 10 条，本工具内置翻页逻辑，会拉满全部渠道。如手动排查，请加 `?p=1&page_size=100`。

**Q：视频模型一直失败怎么办？**
A：new-api 视频模型对 `GET /api/channel/test/:id` 不支持（本就会返回 `channel test is not supported`），属预期内错误，自动跳过，不告警。如果你不希望它在库里产生噪声记录，可以把视频渠道改成 `status = 2` 让程序直接跳过。

**Q：为什么不直接重试视频/限流这类预期内错误？**
A：重试对预期内错误无收益（视频模型按测试接口就是会一直返回 `not supported`，429 一般是账号级 RPM 限）。直接跳过更省时间，避免每轮检测都白白等若干次 `retry_delay_seconds`。

## 许可

[MIT](./LICENSE)
