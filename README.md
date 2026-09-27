# channel-monitor

> 零入侵式 new-api 渠道可用性监控：自动拉取启用渠道、逐模型健康检测、失败重试、飞书告警、SQLite 留痕。

[![Go Version](https://img.shields.io/badge/go-1.22+-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](./LICENSE)
[![Docker Image](https://img.shields.io/badge/docker-%3Cdocker--hub--user%3E%2Fnew--api--channel--monitor-blue)](https://hub.docker.com/r/<docker-hub-user>/new-api-channel-monitor)

## 它在做什么

`channel-monitor` 周期性地检测 [new-api](https://github.com/songquanpeng/one-api) 各渠道下模型是否还能正常响应，自动完成：

1. 用 dashboard 账号登录 new-api 拿 `access_token`（不依赖调用方接口改造）
2. 分页拉取所有启用渠道（`status=1`）
3. 逐模型调用 `GET /api/channel/test/:id?model=xxx`
4. 失败按配置重试；预期内错误（视频模型不支持测试、价格未配置、上游 429）自动跳过不告警
5. 检测结果入库 SQLite，可按时间段查可用率

适合需要「渠道挂了我第一时间知道」又不希望侵入业务数据库或改 new-api 源码的团队。

## 特性

- **零入侵**：仅调用 new-api 公开 HTTP API，不动业务代码与数据库
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

选择你最顺手的一种方式（推荐环境变量，容器/CI 友好）。

**环境变量方式：**

```bash
cp .env.example .env
# 用编辑器改 .env，至少填 DASHBOARD_USERNAME / DASHBOARD_PASSWORD
```

**JSON 文件方式：**

```bash
cp config.json.example config.json
# 用编辑器改 config.json，至少填 dashboard_username / dashboard_password
```

### 2. 跑一轮试试

```bash
# 环境变量方式
./channel-monitor -once

# JSON 文件方式
./channel-monitor -config config.json -once
```

### 3. 上常驻

```bash
./channel-monitor          # 按 .env 配置的间隔循环检测
# 或
./channel-monitor -config config.json
```

### 4. 查历史可用率

```bash
./channel-monitor -query 24             # 最近 24 小时可用率统计
./channel-monitor -query 24 -failures   # 同时列出失败明细
```

## 配置

### 配置来源与优先级

```
环境变量  →  --config 指定文件  →  config.json.example  →  代码内默认值
```

同一字段在多处出现时，环境变量优先级最高。典型用法：JSON 写结构与业务默认值，`.env` 写账号密码等敏感信息。

### 配置项

| 字段 | 环境变量 | 说明 | 默认值 |
|------|----------|------|--------|
| `api_base_url` | `API_BASE_URL` | new-api 地址（仅允许 http:// 或 https://） | `http://localhost:3000` |
| `dashboard_username` | `DASHBOARD_USERNAME` | new-api 后台管理员用户名 | 必填 |
| `dashboard_password` | `DASHBOARD_PASSWORD` | new-api 后台管理员密码 | 必填 |
| `feishu_webhook_url` | `FEISHU_WEBHOOK_URL` | 飞书机器人 Webhook；留空则不告警 | 可选 |
| `db_path` | `DB_PATH` | SQLite 数据库路径 | `./monitor.db` |
| `interval_minutes` | `INTERVAL_MINUTES` | 一轮完整检测的间隔分钟数 | 30 |
| `retry_times` | `RETRY_TIMES` | 模型测试失败后的重试次数 | 2 |
| `retry_delay_seconds` | `RETRY_DELAY_SECONDS` | 重试之间的等待秒数 | 30 |
| `timeout_seconds` | `TIMEOUT_SECONDS` | 单模型测试超时秒数 | 60 |
| `alert_debounce_minutes` | `ALERT_DEBOUNCE_MINUTES` | 同渠道同模型告警防抖窗口；0 表示不防抖 | 60 |

`interval_minutes` 是唯一的检测周期配置。程序每隔这么久跑一轮覆盖所有启用渠道的所有模型。

### 同时用环境变量与 JSON

例如 `config.json` 写：

```json
{
  "api_base_url": "http://localhost:3000",
  "interval_minutes": 15
}
```

`.env` 写：

```env
DASHBOARD_USERNAME=admin
DASHBOARD_PASSWORD=your-real-password
```

最终生效的是合并后值。

### JSON 配置示例

`config.json.example`：

```json
{
  "api_base_url": "http://localhost:3000",
  "dashboard_username": "",
  "dashboard_password": "",
  "feishu_webhook_url": "",
  "db_path": "./monitor.db",
  "interval_minutes": 30,
  "retry_times": 2,
  "retry_delay_seconds": 30,
  "timeout_seconds": 60,
  "alert_debounce_minutes": 60
}
```

## 运行

### 命令一览

```bash
channel-monitor -once                  # 只跑一轮检测
channel-monitor -config config.json    # 用 JSON 后台循环
channel-monitor -query 24              # 最近 24 小时可用率
channel-monitor -query 24 -failures    # 同时列出失败明细
```

### Docker

**docker-compose（推荐）：**

```yaml
# 在 channel-monitor/ 下创建 .env 后
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
  -v channel-monitor-data:/app/data \
  <docker-hub-user>/new-api-channel-monitor:latest
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
├── main.go              # 入口、循环调度、查询命令
├── config.go            # 配置结构、env/文件加载、默认值
├── auth.go              # new-api dashboard 登录、access token 缓存
├── api.go               # new-api HTTP 客户端（含分页拉全量渠道）
├── monitor.go           # 检测调度、重试、告警决策（预期内错误判断）
├── feishu.go            # 飞书交互卡片构造与发送
├── storage.go           # SQLite 表结构、可用率查询、告警防抖
├── config.json.example  # JSON 配置模板
├── .env.example         # 环境变量配置模板
├── Dockerfile           # 多阶段构建
├── docker-compose.yml   # 独立参考编排
├── go.mod / go.sum      # Go 模块
└── README.md
```

### 关键约定

- 预期内错误以 `ErrorMsg` 文本特征匹配；具体匹配规则见 `monitor.go::isSkippableError` 的注释
- `status = 1` 才是 new-api 转发时使用的「启用」标志
- 单一进程单 SQLite 连接；检测周期 ≥ 5 分钟避免触发 new-api 登录限流

### 本地编译

```bash
go build ./...
gofmt -l .    # 应无输出
go vet ./...  # 应无警告
```

## 常见问题

**Q：登录频繁触发限流怎么办？**
A：调大 `interval_minutes`，建议 30 分钟以上。本工具会缓存 access_token，token 15 分钟有效期内不会重复登录。

**Q：`GET /api/channel/` 一页只有 10 条，是丢数据了吗？**
A：默认分页就是 10 条，本工具内置翻页逻辑，会拉满全部渠道。如手动排查，请加 `?p=1&page_size=100`。

**Q：视频模型一直失败怎么办？**
A：new-api 视频模型对 `GET /api/channel/test/:id` 不支持（本就会返回 `channel test is not supported`），属预期内错误，自动跳过，不告警。如果你不希望它在库里产生噪声记录，可以把视频渠道改成 `status = 2` 让程序直接跳过。

**Q：为什么不直接重试视频/限流这类预期内错误？**
A：重试对预期内错误无收益（视频模型按测试接口就是会一直返回 `not supported`，429 一般是账号级 RPM 限）。直接跳过更省时间，避免每轮检测都白白等若干次 `retry_delay_seconds`。

## 许可

[MIT](./LICENSE)
