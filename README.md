<div align="center">
  <img src="static/logo.svg" alt="Ebook API Service Logo" width="96" height="96" />
  <h1>Ebook API Service</h1>
  <p>独立的、可扩展多书源的电子书搜索与流式下载 HTTP 服务</p>

  <p>
    <img src="https://img.shields.io/badge/Go-1.24%2B-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go 1.24+" />
    <img src="https://img.shields.io/badge/Dependencies-Zero-2ea44f?style=flat-square" alt="Zero Dependencies" />
    <img src="https://img.shields.io/badge/Crypto-AES--256--GCM-2563eb?style=flat-square" alt="AES-256-GCM" />
    <a href="https://github.com/imlinhanchao/ebook-api/releases"><img src="https://img.shields.io/github/v/release/imlinhanchao/ebook-api?style=flat-square" alt="GitHub Release" /></a>
  </p>

  <p>
    <a href="#核心特性">核心特性</a> ·
    <a href="#快速开始">快速开始</a> ·
    <a href="#环境变量配置">配置说明</a> ·
    <a href="#认证与加密令牌">认证机制</a> ·
    <a href="#api-参考">API 参考</a> ·
    <a href="#cdn-加速与缓存">CDN 加速</a> ·
    <a href="#扩展新书源">扩展书源</a>
  </p>
</div>

---

## 核心特性

- **纯 Go 标准库，零外部依赖**：单一静态二进制即可运行，默认内嵌前端测试台（`go:embed` 兜底），任意工作目录启动均不会 404。
- **插件化多书源架构**：核心 `Provider` 接口与可选能力接口解耦，天然支持单书源搜索与多书源并发聚合搜索（`provider=all` / `providers=a,b`）。
- **无状态加密凭据令牌**：使用 AES-256-GCM（AAD 绑定书源 ID）将上游账号凭据封装为不透明令牌 `ebk1.*`，避免明文凭据在请求头与客户端中暴露，且无需数据库。
- **流式下载与 CDN 友好**：直接流式透传上游文件（不落盘、内置超时与大小限制），提供确定性下载路径、`Cache-Control` 与 `ETag` / `304 Not Modified`，可跨账号共享 CDN 缓存并节省上游每日配额。
- **内置 Web 调试台**：服务启动后访问根路径 `/` 即可在线测试登录、封包、配额校验、查书、详情与下载，配置自动保存于浏览器 `localStorage`。

---

## 内置书源

| 书源 ID | 名称 | 支持能力 | 认证方式 | 说明 |
| --- | --- | --- | --- | --- |
| `zlib` | Z-Library | `search` `detail` `download` `login` `cookies` `quota` `probe` | 邮箱密码 / Cookie | 直连 EAPI 协议，支持多镜像域名健康缓存与自动故障切换 |

> 更多公开或需认证的书源可按 [扩展新书源](#扩展新书源) 章节快速接入。

---

## 快速开始

### 方式一：下载预编译版本（推荐）

从 [GitHub Releases](https://github.com/imlinhanchao/ebook-api/releases) 下载对应平台压缩包并解压：

```bash
tar -xzf ebook-api_v0.1.0_linux_amd64.tar.gz
cd ebook-api_v0.1.0_linux_amd64
cp .env.example .env
./ebook-api
```

### 方式二：源码运行

```bash
git clone https://github.com/imlinhanchao/ebook-api.git
cd ebook-api
cp .env.example .env
make run
# 或构建二进制
make build && ./bin/ebook-api
```

### 方式三：Docker Compose

```bash
cp .env.example .env
docker compose up -d --build
```

启动后默认监听 `http://127.0.0.1:8091`：
- 浏览器访问 **`http://127.0.0.1:8091/`** 打开内置前端测试台。
- 请求 **`GET /health`** 检查服务状态与已启用书源。

---

## 环境变量配置

程序启动时自动读取当前目录的 `.env` 文件（已存在的系统环境变量优先）。

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `EBOOK_ADDR` | `127.0.0.1:8091` | HTTP 监听地址 |
| `EBOOK_DATA_DIR` | `data` | 数据目录（未配置 `EBOOK_SECRET` 时自动持久化 `secret.key`） |
| `EBOOK_STATIC_DIR` | `static` | 前端静态目录（优先读磁盘，找不到自动回退内嵌资源；设为 `off` 关闭 UI） |
| `EBOOK_SECRET` | *自动生成* | 32 字节 AES-256-GCM 密钥（base64 或 hex），用于加解密 `credential_token` |
| `EBOOK_API_KEY` | *空（不鉴权）* | 可选服务级鉴权密钥；设置后除 `/health` 外均需鉴权 |
| `EBOOK_PUBLIC_DOWNLOAD` | `false` | 设为 `true` 时，`GET` 下载接口跳过 `EBOOK_API_KEY` 校验（便于 CDN 回源） |
| `EBOOK_DOWNLOAD_CACHE_CONTROL` | `public, max-age=2592000, immutable` | 下载接口返回的 `Cache-Control` 响应头 |
| `EBOOK_ALLOW_PLAIN_CRED_HEADERS` | `false` | 是否允许明文凭据请求头（生产环境建议保持 `false`） |
| `EBOOK_DEFAULT_PROVIDER` | `zlib` | 搜索未指定 `provider` 时使用的默认书源 |
| `EBOOK_ENABLED_PROVIDERS` | *空（全部启用）* | 启用的书源白名单，逗号分隔（如 `zlib,gutenberg`） |
| `EBOOK_MAX_DOWNLOAD_MB` | `500` | 单文件最大下载大小（MB，`0` 表示不限制） |
| `EBOOK_CORS_ORIGINS` | *空* | 允许的 CORS Origin 列表，逗号分隔（`*` 表示允许全部） |
| `ZLIB_DOMAINS` | `https://z-lib.gd,...` | Z-Library 镜像域名列表，逗号分隔，按优先级排序 |
| `ZLIB_DOMAIN` / `ZLIB_REMIX_USERID` / `ZLIB_REMIX_USERKEY` | *空* | 服务端默认 Z-Library 凭据（请求未携带令牌时兜底） |
| `EBOOK_CRED_<PROVIDER>_<KEY>` | *空* | 任意书源通用默认凭据，例如 `EBOOK_CRED_CUSTOM_API_KEY=xxx` |

---

## 认证与加密令牌

本服务采用**双层分离认证**设计：

### 1. 服务级鉴权（调用方 → `ebook-api`）

配置 `EBOOK_API_KEY` 后，除 `GET /health`（以及开启 `EBOOK_PUBLIC_DOWNLOAD=true` 时的 `GET` 下载接口）外，所有 `/v1/*` 接口均需携带以下任一请求头：

```http
Authorization: Bearer <EBOOK_API_KEY>
# 或
X-API-Key: <EBOOK_API_KEY>
```

### 2. 书源级凭据（无状态 AES-256-GCM 加密令牌）

为避免明文 `remix_userkey` 或账号密码在请求头与代理日志中泄露，书源凭据统一封装为不透明令牌 `ebk1.<base64url>`（AAD 绑定书源 ID，防篡改与跨书源重放）：

1. **换取令牌**：
   - `POST /v1/providers/{id}/login`：提交邮箱与密码登录，直接返回 `credential_token`（不回传明文上游凭据）。
   - `POST /v1/providers/{id}/seal`：提交已有 Cookie/凭据，校验后封装为 `credential_token`。
2. **携带令牌**：
   - 请求头：`X-Provider-Token: ebk1....`
   - 或 `POST` JSON 体：`"credential_token": "ebk1...."`（多书源聚合搜索使用 `"credential_tokens_by_provider": {"zlib": "ebk1...."}`）。
3. **合并优先级**（高优先级覆盖低优先级）：
   `POST credential_token` > `POST credentials` > `X-Provider-Token` > `X-Provider-Domain` > 服务端默认凭据（`.env`）。

---

## API 参考

### 接口一览

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/` | 内置前端测试台 |
| `GET` | `/health` | 健康检查，返回版本号与已启用书源列表 |
| `GET` | `/v1/providers` | 获取所有已注册书源及其能力元数据 |
| `GET` | `/v1/providers/{id}` | 获取单个书源元数据 |
| `POST` | `/v1/providers/{id}/login` | 账密登录，返回加密 `credential_token` 与账号配额 |
| `POST` | `/v1/providers/{id}/seal` | 将已有书源凭据加密封装为 `credential_token` |
| `POST` | `/v1/providers/{id}/verify` | 校验令牌有效性并查询账号配额 |
| `POST` | `/v1/providers/{id}/probe` | 探测书源镜像域名可用性 |
| `GET` / `POST` | `/v1/search` | 搜索图书（支持单源、多源聚合 `providers=a,b` 或 `provider=all`） |
| `GET` / `POST` | `/v1/providers/{id}/books/{ref}` | 获取单本图书详情 |
| `GET` / `POST` | `/v1/providers/{id}/books/{ref}/download` | 流式下载图书文件（附带 `ETag` 与 `Cache-Control`） |

### 调用示例

```bash
# 1. 登录换取加密令牌
TOKEN=$(curl -s -X POST http://127.0.0.1:8091/v1/providers/zlib/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"your-password"}' | jq -r .credential_token)

# 或者：将已有 Cookie 封装为加密令牌
TOKEN=$(curl -s -X POST http://127.0.0.1:8091/v1/providers/zlib/seal \
  -H 'Content-Type: application/json' \
  -d '{"credentials":{"domain":"https://z-lib.gd","remix_userid":"12345","remix_userkey":"abcdef"}}' | jq -r .credential_token)

# 2. 搜索图书（支持 ext / lang / year_from / year_to / page / limit / order）
curl -s "http://127.0.0.1:8091/v1/search?q=%E4%B8%89%E4%BD%93&provider=zlib&ext=epub&limit=10" \
  -H "X-Provider-Token: $TOKEN"

# 3. 获取图书详情
curl -s "http://127.0.0.1:8091/v1/providers/zlib/books/123456:abcdef" \
  -H "X-Provider-Token: $TOKEN"

# 4. 流式下载图书
curl -L -J -O "http://127.0.0.1:8091/v1/providers/zlib/books/123456:abcdef/download" \
  -H "X-Provider-Token: $TOKEN"
```

### 错误响应格式

所有非 2xx 响应均返回统一 JSON 结构：

```json
{
  "error": {
    "code": "SESSION_EXPIRED",
    "message": "书源会话已失效，请重新登录或更新凭据"
  }
}
```

| HTTP 状态码 | 错误码 (`code`) | 含义 |
| --- | --- | --- |
| `400` | `BAD_REQUEST` / `INVALID_INPUT` / `INVALID_CREDENTIALS` / `UNSUPPORTED` | 参数错误、令牌无效或书源不支持该操作 |
| `401` | `UNAUTHORIZED` / `SESSION_EXPIRED` | API Key 无效或上游书源会话过期 |
| `403` | `QUOTA_EXCEEDED` | 上游账号今日下载配额已用尽 |
| `404` | `PROVIDER_NOT_FOUND` / `NOT_FOUND` | 书源或图书不存在 |
| `409` | `PROVIDER_NOT_CONFIGURED` | 缺少书源凭据配置 |
| `502` | `PROVIDER_UNAVAILABLE` / `PROVIDER_ERROR` | 上游书源不可达或返回异常 |
| `503` | `PROVIDER_BLOCKED` | 镜像触发反爬拦截，建议切换域名重试 |

---

## CDN 加速与缓存

同一本书的下载路径 `/v1/providers/{id}/books/{ref}/download` 在不同账号下完全一致，天然适合接入 CDN 缓存，**命中缓存后不仅显著提速，还不再消耗上游每日下载额度**：

1. 在 `.env` 中配置默认回源凭据（如 `ZLIB_REMIX_USERID` / `ZLIB_REMIX_USERKEY`），并视需要开启 `EBOOK_PUBLIC_DOWNLOAD=true`。
2. 在 CDN 侧将回源路径 `/v1/providers/*/books/*/download` 配置为**忽略请求头、按纯 URL 路径缓存**。
3. 服务端默认返回 `Cache-Control: public, max-age=2592000, immutable` 及确定性 `ETag`，并支持 `If-None-Match` 直接返回 `304 Not Modified`。

---

## 部署与发布

### systemd 部署（Linux）

仓库提供模板 [deploy/ebook-api.service](deploy/ebook-api.service)，替换 `__APP_DIR__` 为实际部署路径后启用即可：

```bash
sed "s|__APP_DIR__|/opt/ebook-api|g" deploy/ebook-api.service > ~/.config/systemd/user/ebook-api.service
systemctl --user daemon-reload
systemctl --user enable --now ebook-api
```

### GitHub Actions 手动发布

仓库内置 [.github/workflows/release.yml](.github/workflows/release.yml)，在 GitHub **Actions → Release → Run workflow** 中填写版本号（如 `v0.1.0`），即可自动运行测试、交叉编译 5 个平台（Linux/macOS/Windows）并发布至 GitHub Releases（附 `checksums.txt`）。

---

## 扩展新书源

1. 在 `internal/providers/<source>/` 下新建包并实现核心接口 `providers.Provider`：
   ```go
   type Provider interface {
       ID() string
       DisplayName() string
       Capabilities() []string
       Search(ctx context.Context, creds Credentials, q SearchQuery) (SearchResult, error)
       GetBook(ctx context.Context, creds Credentials, ref string) (Book, error)
       OpenDownload(ctx context.Context, creds Credentials, ref string) (*DownloadStream, error)
   }
   ```
2. 按需实现可选能力接口（公开免登录书源无需实现认证接口）：
   - `providers.InfoProvider`（书源元数据、`AuthRequired`、`CredentialKeys`）
   - `providers.Verifier`（凭据校验与配额查询）
   - `providers.PasswordAuthenticator`（账密登录）
   - `providers.DomainProber`（镜像健康探测）
   - `providers.FileDownloader`（落盘下载）
3. 在 `cmd/ebook-api/main.go` 的 `registerProviders` 中注册即可；加密令牌（`/seal`、`X-Provider-Token`）、多源聚合搜索、前端测试台表单与通用环境变量 `EBOOK_CRED_<PROVIDER>_<KEY>` 均自动生效。

---

## 项目结构

```text
ebook-api/
├── cmd/ebook-api/          # 服务入口与书源注册
├── internal/
│   ├── api/                # HTTP 路由、中间件、搜索/下载/认证处理
│   ├── config/             # 环境变量与 .env 加载
│   ├── secret/             # AES-256-GCM 加密令牌封装与解封
│   └── providers/          # 书源抽象接口与注册表
│       └── zlib/           # Z-Library EAPI 书源实现
├── static/                 # 内置前端测试台（支持 go:embed 兜底）
├── deploy/                 # systemd 服务模板
├── .github/workflows/      # 手动触发的多平台 Release 发布工作流
├── Dockerfile
├── docker-compose.yml
└── Makefile
```
