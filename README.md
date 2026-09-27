# 渠道可用性监控（零入侵）

独立的 Go 工具，定时检测 new-api 渠道下各模型可用性，失败重试后推送到飞书，结果存入本地 SQLite。

## 特性

- **零入侵**：不修改 new-api 任何代码，只调用现有 HTTP API
- **自动拉取渠道**：通过 dashboard 账号自动登录后调用 `GET /api/channel/` 获取全部渠道，支持分页自动翻页拉取完整列表，无需手动同步
- **自动跳过停用渠道**：只检测 new-api 中状态为「启用」的渠道（`status = 1`），手动停用（`2`）或自动停用（`3`）的渠道自动跳过
- **模型级检测**：遍历渠道 `models` 字段，逐个调用 `GET /api/channel/test/:id?model=xxx`
- **失败重试**：失败后可配置重试次数和重试间隔，默认重试 2 次（共 3 次）；**但预期内错误（视频模型不支持测试、模型未配置价格、上游限流 429）直接跳过，不重试**
- **预期内错误跳过**：通过响应 `message` 文本特征识别三类预期内错误，跳过告警且不重试：
  - 不支持测试的渠道类型（如视频模型）—— message 含 `channel test is not supported`
  - 模型未配置价格 —— message 含 `价格未配置` / `price not configured`
  - 上游限流 429 —— message 含 `status code 429` / `rate limit` / `too many requests` 等
- **飞书告警**：失败重试后仍无法解决则推送 Lark 交互卡片（预期内错误除外）
- **告警防抖**：同一渠道同一模型在防抖窗口内只告警一次
- **可用率查询**：命令行查询最近 N 小时可用率
- **优雅退出**：支持 `SIGINT` / `SIGTERM`，重试等待期间可立即取消

## 目录结构

```
channel-monitor/
├── main.go          # 入口、循环调度、查询命令
├── config.go        # 配置结构与默认值（支持 env 覆盖）
├── auth.go          # new-api dashboard 登录与 access token 缓存
├── api.go           # new-api HTTP 客户端（支持分页拉全量渠道）
├── monitor.go       # 检测调度、重试、告警决策（含预期内错误判断）
├── feishu.go        # 飞书 Webhook 卡片
├── storage.go       # SQLite 读写
├── config.json.example  # JSON 配置模板（不入仓）
├── .env.example     # 环境变量配置模板（不入仓）
├── Dockerfile       # 多阶段构建
├── docker-compose.yml # 独立参考编排
├── .gitignore       # 排除 .env、config.json、monitor.db、channel-monitor.exe
├── go.mod / go.sum  # Go 模块
└── README.md
```

## 认证方式

`/api/channel/` 等管理接口只接受 dashboard 凭证，不接受普通 relay token（`sk-...`）。

本工具采用以下零入侵方式获取管理权限：

1. 启动时用 `dashboard_username` / `dashboard_password` 调用 `POST /api/user/login`
2. 登录成功后返回 `access_token`（dashboard JWT，有效期 15 分钟）
3. `AuthManager` 缓存 access token，token 即将过期时才重新登录
4. 用 `access_token` 调用 `GET /api/channel/` 获取启用渠道列表
5. 用 `access_token` 调用 `GET /api/channel/test/:id?model=xxx` 测试模型

> 检测频率通常设置为 30 分钟或 1 小时一次，access token 15 分钟有效期内只需登录 1 次，避免触发 new-api 登录限流。

## 渠道拉取与分页

`GET /api/channel/` 是分页接口，默认每页 10 条（服务端 `ItemsPerPage`），`page_size` 上限 100。

本工具拉取逻辑（`api.go` 的 `ListChannels`）：

- 每次请求固定携带 `p=<页码>` 与 `page_size=100`，从第 1 页开始
- 每页解析 `data.items`，累加到本地列表
- 当累计数量 ≥ `data.total`，或本页返回条数 < 100（说明已是最后一页）时停止翻页
- 因此无论部署多少渠道（10 个、14 个还是 300 个），都能拉到完整列表

> **排查提示**：如果手动用 curl/jq 调 `GET /api/channel/` 不带分页参数，只能看到第一页 10 条 —— 这是接口默认分页，不代表渠道丢失。加 `?p=1&page_size=100` 即可一次看全（单页最多 100 条）。

## 渠道状态与启用判断

new-api 渠道状态定义（与 new-api 侧 `common/constants.go` 一致）：

| status | 含义 | 是否检测 |
|--------|------|----------|
| `1` | 启用 | ✅ 检测 |
| `2` | 手动停用 | ❌ 跳过 |
| `3` | 自动停用 | ❌ 跳过 |

`status = 1` 是 new-api 自身路由转发时使用的同一个启用标志（数据库 abilities 表的 `enabled` 字段由它派生），因此本工具的过滤结果与「哪些渠道真实接收流量」一致。

> **排查提示**：前端渠道列表页和 `GET /api/channel/`（不带 `status` 参数）默认返回**所有状态**的渠道。判断「实际启用了几个渠道」请用：
>
> ```bash
> curl -s "http://<new-api>/api/channel/?p=1&page_size=100" \
>   | jq '[.data.items[] | select(.status == 1)] | length'
> ```

## 监控流程

```
启动
  │
  ▼
每隔 interval_minutes 执行一轮检测
  │
  ▼
POST /api/user/login（获取并缓存 access_token）
  │
  ▼
GET /api/channel/?p=1&page_size=100（分页拉取全部渠道）
  │
  ▼
过滤 status = 1 的启用渠道
  │
  ▼
对每个启用渠道的每个模型：
  ├─ 调用 GET /api/channel/test/:id?model=xxx
  ├─ 失败时等待 retry_delay_seconds 后重试，最多 retry_times 次
  │  （预期内错误不重试，直接跳过）
  ├─ 重试后仍失败 → 记录 SQLite + 触发飞书告警（去抖）
  ├─ 预期内错误（不支持测试/未配置价格/上游限流 429）→ 记录 SQLite，不告警
  └─ 成功 → 记录延迟到 SQLite
  │
  ▼
本轮完成，等待下一个 interval_minutes
```

每轮开始时会打印 `[开始] 共 N 个启用渠道`，N 即本轮实际检测的渠道数量；被跳过的渠道不会出现在日志中。

## 配置

工具的私有配置（账号密码、飞书 webhook）**不进入仓库**。仓库内只跟踪模板文件 `config.json.example` 与 `.env.example`，请按以下任一方式填写后启动：

### 方式 A：使用环境变量（推荐）

```bash
cp .env.example .env
# 编辑 .env，填入 DASHBOARD_PASSWORD、FEISHU_WEBHOOK_URL 等真实值
./channel-monitor -once
```

`.env` 已被 `.gitignore` 排除，不会被提交。

### 方式 B：使用 JSON 文件

```bash
cp config.json.example config.json
# 编辑 config.json，填入 dashboard_password、feishu_webhook_url 等
./channel-monitor -config config.json -once
```

`config.json` 同样已被 `.gitignore` 排除。

### 配置加载顺序

```
环境变量  >  --config 指定文件  >  config.json.example  >  代码内默认值
```

实现见 `config.go::loadConfig`：先用 `--config` 指定的文件；文件不存在则回退到 `config.json.example`；再之后用同名环境变量覆盖非空字段。

### 同时配置环境变量与 JSON 时

同名字段**环境变量覆盖 JSON**。典型用法：JSON 放结构，`.env` 放密钥。

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

最终生效的就是合并值。

### 配置项说明

| JSON 字段 | 环境变量 | 说明 | 默认值 |
|------|------|--------|--------|
| `api_base_url` | `API_BASE_URL` | new-api 地址（仅允许 http:// 或 https://） | `http://localhost:3000` |
| `dashboard_username` | `DASHBOARD_USERNAME` | new-api 后台管理员用户名 | 必填 |
| `dashboard_password` | `DASHBOARD_PASSWORD` | new-api 后台管理员密码 | 必填 |
| `feishu_webhook_url` | `FEISHU_WEBHOOK_URL` | 飞书机器人 Webhook | 可选，留空不告警 |
| `db_path` | `DB_PATH` | SQLite 数据库路径 | 可执行文件同目录 `monitor.db` |
| `interval_minutes` | `INTERVAL_MINUTES` | 每隔多少分钟执行一轮完整检测 | 30 |
| `retry_times` | `RETRY_TIMES` | 模型测试失败后的重试次数 | 2 |
| `retry_delay_seconds` | `RETRY_DELAY_SECONDS` | 每次重试之间的等待秒数 | 30 |
| `timeout_seconds` | `TIMEOUT_SECONDS` | 单模型测试超时秒数 | 60 |
| `alert_debounce_minutes` | `ALERT_DEBOUNCE_MINUTES` | 同一渠道模型告警防抖（分钟），0 表示不防抖 | 60 |

`interval_minutes` 是唯一的检测周期配置。程序会每隔这么多分钟执行一轮检测，每轮检测所有启用渠道的所有模型。

## 运行

`channel-monitor` 支持两种配置来源，命令行会优先用 `--config` 指定的文件：

```bash
# 方式 A：用环境变量（推荐，本地/CI/容器通用）
export DASHBOARD_PASSWORD=xxx
export FEISHU_WEBHOOK_URL=https://...
./channel-monitor -once

# 方式 B：用 JSON 文件
./channel-monitor -config config.json -once
```

### 后台循环（按 interval_minutes 周期检测）

```bash
./channel-monitor -config config.json
```

### 只运行一轮

```bash
./channel-monitor -once
```

### 查询可用率

```bash
# 最近 24 小时可用率
./channel-monitor -query 24

# 同时显示失败明细
./channel-monitor -query 24 -failures
```

### Docker / docker-compose

`docker-compose.yml` 已改用 `env_file` 注入 `.env`，无需挂载 config 文件：

```bash
# 1. 在 channel-monitor/ 下创建并填写 .env（参考 .env.example）
cp .env.example .env && vim .env

# 2. 启动
docker compose up -d channel-monitor

# 3. 容器内查询可用率
docker exec new-api-channel-monitor /app/channel-monitor -query 24 -failures
```

如要使用 JSON 而非 env：

```bash
docker run --rm -it \
  -v "$PWD/config.json:/etc/channel-monitor/config.json:ro" \
  -v channel-monitor-data:/app/data \
  calciumion/new-api-channel-monitor:latest -config /etc/channel-monitor/config.json
```

## 定时任务示例

### Linux / macOS（systemd timer）

`~/.config/systemd/user/channel-monitor.service`：

```ini
[Unit]
Description=new-api channel monitor

[Service]
Type=simple
ExecStart=/opt/new-api/channel-monitor/channel-monitor -config /opt/new-api/channel-monitor/config.json
Restart=always
WorkingDirectory=/opt/new-api/channel-monitor
```

启动：

```bash
systemctl --user daemon-reload
systemctl --user enable --now channel-monitor.service
```

### Windows 任务计划程序

```powershell
$action = New-ScheduledTaskAction -Execute "C:\path\to\channel-monitor.exe" -Argument "-config C:\path\to\config.json"
$trigger = New-ScheduledTaskTrigger -Once -At (Get-Date) -RepetitionInterval (New-TimeSpan -Minutes 30)
Register-ScheduledTask -Action $action -Trigger $trigger -TaskName "NewAPI-ChannelMonitor" -User "SYSTEM"
```

### 镜像说明

`channel-monitor/Dockerfile` 已提供多阶段构建：

1. 使用 `golang:1.26.1-alpine` 编译
2. 使用阿里云 Go 模块代理（`GOPROXY=https://mirrors.aliyun.com/goproxy/,direct`）
3. 复制到 `alpine:latest` 运行时镜像
4. 默认从 `/app/config/config.json` 读取配置，数据库写入 `/app/data/monitor.db`

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
```

## 常见排查

| 现象 | 原因与处理 |
|------|-----------|
| 手动 `GET /api/channel/` 只看到 10 个渠道 | 接口默认分页每页 10 条，加 `?p=1&page_size=100` 查看全部 |
| monitor 检测的渠道数比前端显示的少 | 前端/列表接口默认显示所有状态的渠道；monitor 只检测 `status=1` 的启用渠道，属预期行为 |
| 确认 `status=1` 的渠道数仍与 monitor 检测数不符 | 查看每轮日志第一行 `[开始] 共 N 个启用渠道`；再确认 `api_base_url` 指向的实例与预期一致（Docker 内网地址与宿主机端口可能不是同一实例） |
| 渠道测试失败但未收到飞书告警 | 检查 `feishu_webhook_url` 是否为空；该模型是否命中预期内错误（不支持测试/未配置价格/上游限流 429）；是否处于防抖窗口内 |
| 视频模型挂在 VolcEngine 等可测试渠道下，测试报 429 `ModelAccountRpmRateLimitExceeded` | 预期内：new-api 对视频模型发 chat 探针，上游按账号级模型 RPM 限流。monitor 已将上游限流 429 列入预期内错误，不重试不告警，仅落库 |
| 前端单渠道「测试」按钮通过，但 monitor 判定失败 | 两者路径不同：前端测试直接按渠道 ID 测试，monitor 逐模型测试并校验响应，个别模型级别失败会体现在明细里 |

## 注意事项

1. `dashboard_username` / `dashboard_password` 需要是拥有 `channel:read` 和 `channel:operate` 权限的管理员账号。
2. 首次启动会立即登录并检测一轮。
3. 只有 new-api 中状态为「启用」（status = 1）的渠道会被检测；停用渠道自动跳过。
4. 每轮检测中，渠道内的模型是串行测试的，避免并发过高压到上游或 new-api。
5. 飞书 Webhook 为空时不会发送告警，但测试记录仍会写入 SQLite。
6. SQLite 默认启用 WAL 模式和 5 秒 busy timeout，支持监控写入和查询命令并发执行。
7. 如果 new-api 登录开启了 Turnstile 验证码，自动登录会失败，需要关闭相关限制。
8. 如果频繁触发 429 限流，请检查 `CRITICAL_RATE_LIMIT_ENABLE` 配置，或清空 Redis 中的 `rateLimit:v2:ip:CT:*` key。

## 实际部署记录

### 认证与账号

- 管理接口 `GET /api/channel/test/:id?model=xxx` 需要管理员的 access token（API key 无效），token 有效期 15 分钟，单轮检测内可测完全部模型
- 在平台中创建了 `channel-monitor` 专用用户并提升为管理员，密码存放在 `config.json` 中

### 构建与分发

```bash
# 本地（构建镜像并传到目标服务器）
cd /home/otto/new-api-r24
sudo docker build --no-cache -t calciumion/new-api-channel-monitor:latest ./channel-monitor
sudo docker save calciumion/new-api-channel-monitor:latest | gzip > channel-monitor.tar.gz
scp -P 31001 channel-monitor.tar.gz ubuntu@113.108.13.167:/opt/newapi/
```

### 目标服务器部署

```bash
cd /opt/newapi
sudo docker load -i channel-monitor.tar.gz
sudo docker stop new-api-channel-monitor
sudo docker rm new-api-channel-monitor
sudo docker run -d \
  --name new-api-channel-monitor \
  --restart always \
  --network test_default \
  --cap-add NET_ADMIN \
  -v /opt/newapi/channel-monitor/config.json:/app/config/config.json:ro \
  -v /opt/newapi/channel-monitor-data:/app/data \
  -e TZ=Asia/Shanghai \
  calciumion/new-api-channel-monitor:latest
```

### 修改容器 MTU

channel-monitor 和 Uptime Kuma 遇到了同样的飞书 webhook 发送失败问题。Uptime Kuma 可以通过 docker-compose.yml 配置 MTU，而 channel-monitor 必须依附于 new-api（`config.json` 的 `api_base_url` 要求两者在同一容器网络，且无法轻易修改 new-api 容器的网络配置），只能进入容器内部修改：

```bash
# 进入容器
docker exec -it new-api-channel-monitor sh

# 修改 MTU（使用 ifconfig，它通常是内置的）
ifconfig eth0 mtu 1450

# 验证是否修改成功
ifconfig eth0 | grep MTU

# 退回主机，查看日志
sudo docker logs -f new-api-channel-monitor
```

> 注意：容器重建（如升级镜像）后 MTU 会恢复默认值，需要重新执行上述修改。

### 为什么跳过图片/视频模型

new-api 暂时不支持对图片、视频模型的测试，只能跳过。测试接口 `GET /api/channel/test/:id?model=xxx` 返回结果中只能拿到 `message`，因此只能通过读取 message 内容，判断是模型本身有问题还是 new-api 不支持测试。判断逻辑见 `monitor.go` 的 `isSkippableError`：

```go
// 判断 message 内容中存在
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
```
