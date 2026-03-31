# TempMail (轻量版)

一个自托管临时邮箱服务平台，**使用 SQLite 替代 PostgreSQL + Redis**，仅需 1 个容器即可运行。

支持多域名、用户自助提交域名、MX 自动验证与自动禁用、API Key 鉴权及 Web 管理后台。

---

## 功能特性

| 功能 | 说明 |
|------|------|
| 邮箱管理 | 按需创建临时邮箱，可配置 TTL（默认 30 分钟），自动清理 |
| 多域名池 | 多个域名轮流供用户创建邮箱，管理员或普通用户均可提交新域名 |
| MX 自动验证 | 提交域名后后台每 30 秒轮询 MX 记录，通过即自动激活 |
| 域名健康监控 | 每 6 小时重检已激活域名，MX 失效自动暂停 |
| IP / Hostname 分离 | 服务器 IP 与邮件主机名通过环境变量注入 |
| API Key 鉴权 | 每用户独立 API Key，内存速率限制 500 次/分钟 |
| 管理后台 | Web GUI 管理账户、域名、邮件、系统配置 |
| Dashboard 统计 | 实时展示邮箱数、邮件数、域名数、账户数 |
| 公告系统 | 管理员可设置公告，用户登录后显示 |
| 轻量部署 | SQLite 单文件数据库，无需 PostgreSQL/PgBouncer/Redis |

---

## 快速启动

### 前置条件

- Docker 20.10+
- Docker Compose v2+
- 公网 IP / 域名（用于接收邮件）

### 1. 克隆并配置

```bash
git clone <repo-url>
cd tempmail
cp .env.example .env
# 编辑 .env，填写 SMTP_SERVER_IP 和 SMTP_HOSTNAME
```

### 2. 启动服务

```bash
docker compose up -d --build
```

单个容器内通过 Nginx 提供前端静态文件并反向代理 API 请求，Go API 和 Postfix 在后台运行，对外仅暴露 Web 端口和 SMTP 端口。

> SQLite 数据库文件存储在 `./data/tempmail.db`，首次启动自动创建。

### 3. 获取管理员 API Key

首次启动后，管理员 Key 会写入 `data/admin.key`：

```bash
cat data/admin.key
```

### 4. 访问 Web 界面

浏览器打开 `http://<服务器IP>`，在登录页输入管理员 API Key 登录。

---

## 环境变量

在项目根目录 `.env` 文件中配置：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `SMTP_SERVER_IP` | *(必填)* | 服务器公网 IP |
| `SMTP_HOSTNAME` | *(推荐)* | 邮件服务器主机名 |
| `API_DB_PATH` | `/data/tempmail.db` | SQLite 文件路径 |
| `API_RATE_LIMIT` | `500` | 每令牌每窗口期最大请求数 |
| `API_RATE_WINDOW` | `60` | 速率窗口（秒）|

`.env` 示例：

```dotenv
SMTP_SERVER_IP=1.2.3.4
SMTP_HOSTNAME=mail.yourdomain.com
```

---

## 添加邮件域名

任意已登录用户均可提交域名，管理员可在后台直接添加。

### 所需 DNS 记录

**已配置 `SMTP_HOSTNAME`（推荐）**——仅需 2 条：

```
MX   @   mail.yourdomain.com   优先级 10
TXT  @   v=spf1 ip4:<服务器IP> ~all
```

**未配置 `SMTP_HOSTNAME`**——需 3 条：

```
MX   @              mail.example.com   优先级 10
A    mail           <服务器公网 IP>
TXT  @              v=spf1 ip4:<服务器公网 IP> ~all
```

---

## API 使用

所有 API 请求需在 Header 携带：

```
X-API-Key: tm_xxxxxxxxxxxx
```

```bash
BASE="http://<服务器IP>"
KEY="your_api_key"

# 获取可用域名（无需登录）
curl "$BASE/public/domains"

# 创建邮箱
curl -X POST "$BASE/api/mailboxes" \
  -H "X-API-Key: $KEY" \
  -H "Content-Type: application/json" \
  -d '{"address":"test","domain_id":"<domain-uuid>"}'

# 列出邮箱
curl "$BASE/api/mailboxes" -H "X-API-Key: $KEY"

# 读取邮件
curl "$BASE/api/mailboxes/<mailbox-id>/emails" -H "X-API-Key: $KEY"

# 获取统计
curl "$BASE/public/stats"
```

---

## 项目结构

```
tempmail/
├── api/                  # Go API 服务
│   ├── main.go           # 路由、中间件、后台 goroutine
│   ├── config/           # 环境变量配置
│   ├── handler/          # HTTP 处理器
│   ├── middleware/        # 鉴权、速率限制（内存实现）
│   ├── model/            # 数据结构
│   └── store/            # SQLite 数据库操作
├── frontend/             # 静态 SPA（由 Nginx 提供服务）
├── nginx/                # Nginx 配置（前端静态文件 + API 反向代理）
├── postfix/              # Postfix 邮件接收
├── sql/                  # SQLite DDL 参考
├── data/                 # 运行时数据（tempmail.db、admin.key）
├── Dockerfile            # 单容器构建（Nginx + Go + Postfix + supervisord）
├── supervisord.conf      # 进程管理配置（nginx + api-server + postfix）
├── docker-compose.yml
└── .env
```

---

## 许可证

MIT
