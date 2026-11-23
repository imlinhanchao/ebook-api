# ebook-api

独立的、可扩展多书源的电子书搜索与下载 HTTP 服务（纯 Go 标准库，零外部依赖）。

当前内置书源：
- **`zlib` (Z-Library)**：直接通过 EAPI 协议通信，支持镜像域名故障切换、登录换取加密令牌、账号配额查询、搜索、详情、流式下载。

## 快速开始

```bash
cp .env.example .env
make run
# 或构建二进制
make build && ./bin/ebook-api
```

默认监听 `127.0.0.1:8091`。首次启动如未配置 `EBOOK_SECRET`，会自动在 `data/secret.key` 生成并持久化 32 字节 AES-256-GCM 密钥。

## 鉴权与加密凭据令牌（方案 A）

### 1. 服务级鉴权（可选）
配置 `EBOOK_API_KEY` 后，除 `/health` 外所有接口均需携带：
- `Authorization: Bearer <EBOOK_API_KEY>` 或
- `X-API-Key: <EBOOK_API_KEY>`

### 2. 书源凭据：无状态 AES-256-GCM 加密令牌 (`credential_token`)
为避免明文凭据在请求头或客户端中传播，书源凭据采用 **AES-256-GCM 加密密封令牌**（格式 `ebk1.<base64url>`，且通过 AAD 绑定书源 ID，防跨书源重放与篡改）：

1. **获取令牌**（二选一）：
   - `POST /v1/providers/{id}/login`：提交邮箱密码登录，服务端直接返回加密后的 `credential_token`（不回传明文 `remix_userkey`）。
   - `POST /v1/providers/{id}/seal`：如果你已有现成 Cookie/凭据，提交一次换取加密 `credential_token`（默认同时校验账号有效性）。
2. **使用令牌**：
   - `GET` / `POST` 请求头：`X-Provider-Token: ebk1....`
   - 或 `POST` 请求体：`"credential_token": "ebk1...."`（多书源聚合搜索支持 `credential_tokens_by_provider`）。
3. **兜底配置**：
   - 未传令牌时，回退使用服务端 `.env` 中配置的默认凭据（`ZLIB_*` 或通用 `EBOOK_CRED_<PROVIDER>_<KEY>`）。
   - 明文凭据请求头（`X-Provider-Credentials` / `X-Provider-Cred-*`）**默认禁用**，仅当显式设置 `EBOOK_ALLOW_PLAIN_CRED_HEADERS=true` 时才允许（不建议生产开启）。

## API 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/health` | 健康检查（返回已启用书源列表） |
| `GET` | `/v1/providers` | 列出所有已注册书源及其能力元数据 |
| `GET` | `/v1/providers/{id}` | 获取单个书源信息 |
| `POST` | `/v1/providers/{id}/login` | 账号密码登录，返回加密 `credential_token` 与 `account` |
| `POST` | `/v1/providers/{id}/seal` | 将已有书源凭据加密封装为 `credential_token` |
| `POST` | `/v1/providers/{id}/verify` | 校验令牌/凭据并查询账号状态与配额 |
| `POST` | `/v1/providers/{id}/probe` | 探测镜像域名可用性 |
| `GET` / `POST` | `/v1/search` | 查书（支持单书源、多书源并发聚合 `providers=a,b` 或 `provider=all`） |
| `GET` / `POST` | `/v1/providers/{id}/books/{ref}` | 获取单本图书详情 |
| `GET` / `POST` | `/v1/providers/{id}/books/{ref}/download` | 直接流式下载图书文件 |

### 调用示例

```bash
# 1. 登录换取加密令牌
TOKEN=$(curl -s -X POST http://127.0.0.1:8091/v1/providers/zlib/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"your-password"}' | jq -r .credential_token)

# 或者：用已有 Cookie 换取加密令牌
TOKEN=$(curl -s -X POST http://127.0.0.1:8091/v1/providers/zlib/seal \
  -H 'Content-Type: application/json' \
  -d '{"credentials":{"domain":"https://z-lib.gd","remix_userid":"12345","remix_userkey":"abcdef"}}' | jq -r .credential_token)

# 2. 使用加密令牌查书（GET + X-Provider-Token）
curl -s "http://127.0.0.1:8091/v1/search?q=%E4%B8%89%E4%BD%93&provider=zlib&ext=epub" \
  -H "X-Provider-Token: $TOKEN"

# 3. 使用加密令牌流式下载
curl -L -J -O "http://127.0.0.1:8091/v1/providers/zlib/books/123456:abcdef/download" \
  -H "X-Provider-Token: $TOKEN"
```

## 扩展新书源

1. 在 `internal/providers/<name>/` 下新建包，实现核心接口 `providers.Provider`：
   - `ID() string`
   - `DisplayName() string`
   - `Capabilities() []string`
   - `Search(ctx, creds, SearchQuery) (SearchResult, error)`
   - `GetBook(ctx, creds, ref) (Book, error)`
   - `OpenDownload(ctx, creds, ref) (*DownloadStream, error)`
2. 按需实现可选能力接口（无账号公开书源无需实现认证接口）：
   - `providers.InfoProvider`、`providers.Verifier`、`providers.PasswordAuthenticator`、`providers.DomainProber`、`providers.FileDownloader`
3. 在 `cmd/ebook-api/main.go` 的 `registerProviders` 中注册即可。加密令牌 `/seal` 与 `X-Provider-Token` 对所有书源自动生效。
