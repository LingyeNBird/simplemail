# TempMail（轻量版）

一个自托管临时邮箱平台，采用 **单容器部署**：容器内运行 **Nginx + Go API + Postfix + Supervisord**，数据存储使用 **SQLite**。

当前项目提供 Web 管理界面、API Key 鉴权、临时邮箱收件、域名池与 MX 自动验证、管理员设置面板，以及留存邮件查看能力。

---

## 功能特性

| 功能 | 说明 |
|------|------|
| 临时邮箱 | 创建临时邮箱，默认 TTL 30 分钟；已过期邮箱可手动续期，后台每分钟自动清理过期邮箱 |
| 多域名池 | 支持多个激活域名，创建邮箱时可指定域名，也可随机分配 |
| 域名提交与 MX 验证 | 登录用户可提交域名；后台每 30 秒轮询待验证域名，验证通过后自动激活 |
| 域名健康检查 | 每 6 小时重检已激活域名，MX 失效会自动停用 |
| API Key 鉴权 | 使用 `Authorization: Bearer <API_KEY>`，也支持 `?api_key=` 查询参数 |
| 用户注册开关 | 支持公开注册，可由管理员在系统设置中开启或关闭 |
| 管理后台 | 管理账户、域名、系统设置、公告、默认域名、TTL 等 |
| 留存邮件 | 对不存在收件邮箱的来信进行留存，管理员可在后台查看 |
| Cloudflare 辅助接口 | 管理端内置 Cloudflare 域名相关操作接口 |
| 轻量部署 | SQLite 单文件存储，无需 PostgreSQL、Redis 或额外前端构建容器 |

---

## 部署架构

- **Nginx**：监听容器内 `8080`，对外映射为 `80`，负责静态前端和 API 反向代理
- **Go API**：监听 `127.0.0.1:8081`，仅容器内访问
- **Postfix**：监听 `25`，接收邮件后通过内部投递接口写入系统
- **Supervisord**：统一拉起并管理上述进程，以及域名同步脚本

对外默认暴露端口：

- `80`：Web 界面 / 反代 API
- `25`：SMTP 收件

---

## 快速启动

### 前置条件

- Docker 20.10+
- Docker Compose v2+
- 可接收邮件的公网 IP

### 1. 克隆并配置

```bash
git clone <repo-url>
cd tempmail
cp .env.example .env
```

编辑 `.env`，至少填写：

- `SMTP_SERVER_IP`：服务器公网 IP
- `SMTP_HOSTNAME`：邮件主机名，例如 `mail.example.com`

### 2. 启动服务

```bash
docker compose up -d --build
```

首次启动时会自动：

- 初始化 SQLite 数据库
- 创建默认管理员 API Key
- 启动 Nginx、Go API、Postfix 和域名同步脚本

SQLite 默认存储路径为：

```text
/data/tempmail.db
```

在宿主机上默认映射到：

```text
./data/tempmail.db
```

### 3. 获取管理员 API Key

启动后，管理员 API Key 会写入 `data/admin.key`，文件内容类似：

```dotenv
ADMIN_API_KEY=tm_xxxxxxxxxxxx
```

例如：

```bash
cat data/admin.key
```

### 4. 访问 Web 界面

浏览器打开：

```text
http://<服务器IP>
```

然后使用管理员 API Key 登录。

---

## 环境变量

用户通常只需要编辑根目录 `.env` 文件。当前模板如下：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `SMTP_SERVER_IP` | *(必填)* | 服务器公网 IP，用于 MX 校验和 SPF 相关提示 |
| `SMTP_HOSTNAME` | *(推荐)* | 邮件服务器主机名，例如 `mail.example.com` |
| `API_DB_PATH` | `/data/tempmail.db` | SQLite 文件路径 |
| `API_RATE_LIMIT` | `500` | 每令牌每窗口期最大请求数 |
| `API_RATE_WINDOW` | `60` | 速率窗口（秒） |

`.env.example` 示例：

```dotenv
SMTP_SERVER_IP=1.2.3.4
SMTP_HOSTNAME=mail.yourdomain.com
API_DB_PATH=/data/tempmail.db
API_RATE_LIMIT=500
API_RATE_WINDOW=60
```

说明：

- Compose 会把 `API_DB_PATH / API_RATE_LIMIT / API_RATE_WINDOW` 转换注入为容器内运行时使用的 `DB_DSN / RATE_LIMIT / RATE_WINDOW`
- 管理后台修改 `smtp_server_ip` 时，会同步回写到挂载的 `.env` 文件

---

## 添加邮件域名

- 任意**已登录用户**都可以提交域名进行 MX 验证
- 管理员也可以在后台直接添加、启停和批量管理域名

### 所需 DNS 记录

如果已配置 `SMTP_HOSTNAME`（推荐）：

```text
MX   @   mail.yourdomain.com   优先级 10
TXT  @   v=spf1 ip4:<服务器IP> ~all
```

如果未配置 `SMTP_HOSTNAME`，通常还需要为邮件主机名提供 A 记录，例如：

```text
MX   @              mail.example.com   优先级 10
A    mail           <服务器公网 IP>
TXT  @              v=spf1 ip4:<服务器公网 IP> ~all
```

系统会：

- 每 30 秒轮询待验证域名
- 每 6 小时重检已激活域名
- 自动同步激活域名到 Postfix 的接收域名列表

---

## API 使用

### 认证方式

所有受保护 API 默认使用以下请求头：

```http
Authorization: Bearer tm_xxxxxxxxxxxx
```

也支持查询参数：

```text
?api_key=tm_xxxxxxxxxxxx
```

### 常用接口示例

```bash
BASE="http://<服务器IP>"
KEY="tm_xxxxxxxxxxxx"

# 公开配置
curl "$BASE/public/settings"

# 公开统计
curl "$BASE/public/stats"

# 开启公开注册时可用
curl -X POST "$BASE/public/register" \
  -H "Content-Type: application/json" \
  -d '{"username":"demo-user"}'

# 查看当前用户
curl "$BASE/api/me" \
  -H "Authorization: Bearer $KEY"

# 获取可用域名（需登录）
curl "$BASE/api/domains" \
  -H "Authorization: Bearer $KEY"

# 创建邮箱：可指定 address 和 domain；两者都可省略
curl -X POST "$BASE/api/mailboxes" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"address":"test","domain":"example.com"}'

# 随机域名 + 随机地址
curl -X POST "$BASE/api/mailboxes" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{}'

# 列出邮箱
curl "$BASE/api/mailboxes" \
  -H "Authorization: Bearer $KEY"

# 为已过期邮箱续期 30 分钟
curl -X PUT "$BASE/api/mailboxes/<mailbox-id>/renew" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"minutes":30}'

# 查看某个邮箱的邮件
curl "$BASE/api/mailboxes/<mailbox-id>/emails" \
  -H "Authorization: Bearer $KEY"

# 获取受保护统计
curl "$BASE/api/stats" \
  -H "Authorization: Bearer $KEY"
```

### 当前主要路由

公开接口：

- `GET /health`
- `GET /public/settings`
- `POST /public/register`
- `GET /public/stats`

登录后接口：

- `GET /api/me`
- `GET /api/domains`
- `GET /api/domains/:id/status`
- `POST /api/domains/submit`
- `GET /api/stats`
- `POST /api/mailboxes`
- `GET /api/mailboxes`
- `DELETE /api/mailboxes/:id`
- `PUT /api/mailboxes/:id/renew`
- `GET /api/mailboxes/:id/emails`
- `GET /api/mailboxes/:id/emails/:email_id`
- `DELETE /api/mailboxes/:id/emails/:email_id`

管理员接口还包括账户管理、域名管理、系统设置、留存邮件和 Cloudflare 相关操作。

---

## 前端界面

当前静态 SPA 已内置以下页面/能力：

- API Key 登录
- 可选公开注册
- 邮箱总览与快速创建邮箱
- 邮件列表与邮件详情查看
- 域名列表与添加指南
- API 文档页面
- 管理员：账户管理、域名管理、系统设置、留存邮件

---

## 测试

项目当前包含 Go 测试。可在 `api/` 目录执行：

```bash
cd api
go test ./...
```

---

## 项目结构

```text
tempmail/
├── api/                  # Go API 服务
│   ├── main.go           # 路由、后台任务、服务启动
│   ├── config/           # 运行时配置读取
│   ├── handler/          # HTTP 处理器
│   ├── middleware/       # 鉴权、管理员权限、速率限制
│   ├── model/            # 数据模型
│   ├── store/            # SQLite 存储层
│   └── cf/               # Cloudflare 相关客户端/逻辑
├── frontend/             # 静态前端 SPA
├── nginx/                # Nginx 配置（静态资源 + API 反代）
├── postfix/              # Postfix 配置与邮件投递脚本
├── data/                 # 运行时数据（SQLite、admin.key 等）
├── Dockerfile            # 单容器构建
├── docker-compose.yml    # 默认启动配置
├── supervisord.conf      # 进程管理配置
└── .env.example          # 环境变量模板
```

---

## 许可证

MIT
