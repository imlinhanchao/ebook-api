// Package api exposes the HTTP API for ebook-api.
package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/imlinhanchao/ebook-api/internal/config"
	"github.com/imlinhanchao/ebook-api/internal/providers"
	"github.com/imlinhanchao/ebook-api/internal/secret"
)

// Version is the application version; overridden at build time via -ldflags.
var Version = "0.1.0"

// Server serves the ebook-api HTTP endpoints.
type Server struct {
	cfg      *config.Config
	registry *providers.Registry
	mux      *http.ServeMux
}

// New creates a new API server with all routes registered.
func New(cfg *config.Config, reg *providers.Registry) *Server {
	s := &Server{
		cfg:      cfg,
		registry: reg,
		mux:      http.NewServeMux(),
	}
	s.routes()
	return s
}

// Handler returns the root http.Handler wrapped with logging, recovery, CORS,
// and optional API key authentication.
func (s *Server) Handler() http.Handler {
	return s.logging(s.recoverer(s.cors(s.mux)))
}

func (s *Server) routes() {
	m := s.mux

	// Public health check
	m.HandleFunc("GET /health", s.handleHealth)

	// Providers metadata & account helpers
	m.HandleFunc("GET /v1/providers", s.requireAuth(s.handleListProviders))
	m.HandleFunc("GET /v1/providers/{id}", s.requireAuth(s.handleGetProvider))
	m.HandleFunc("POST /v1/providers/{id}/login", s.requireAuth(s.handleLogin))
	m.HandleFunc("POST /v1/providers/{id}/seal", s.requireAuth(s.handleSeal))
	m.HandleFunc("POST /v1/providers/{id}/verify", s.requireAuth(s.handleVerify))
	m.HandleFunc("POST /v1/providers/{id}/probe", s.requireAuth(s.handleProbe))

	// Search & book detail
	m.HandleFunc("GET /v1/search", s.requireAuth(s.handleSearchGET))
	m.HandleFunc("POST /v1/search", s.requireAuth(s.handleSearchPOST))
	m.HandleFunc("GET /v1/providers/{id}/books/{ref}", s.requireAuth(s.handleBookDetailGET))
	m.HandleFunc("POST /v1/providers/{id}/books/{ref}", s.requireAuth(s.handleBookDetailPOST))

	// Book download (streaming)
	m.HandleFunc("GET /v1/providers/{id}/books/{ref}/download", s.requireAuth(s.handleDownloadGET))
	m.HandleFunc("POST /v1/providers/{id}/books/{ref}/download", s.requireAuth(s.handleDownloadPOST))

	// Frontend test UI (static/)
	if staticDir := strings.TrimSpace(s.cfg.StaticDir); staticDir != "" {
		fs := http.FileServer(http.Dir(staticDir))
		m.Handle("GET /static/", http.StripPrefix("/static/", fs))
		m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, staticDir+"/index.html")
		})
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"version":   Version,
		"providers": s.registry.IDs(),
	})
}

// resolveCredentials merges credentials in increasing order of precedence:
//  1. Server default credentials (env / .env)
//  2. Non-secret X-Provider-Domain header
//  3. Plaintext credential headers (ONLY when AllowPlainCredHeaders is true)
//  4. Encrypted X-Provider-Token header (AES-256-GCM bound to providerID)
//  5. POST JSON body credentials
//  6. POST JSON body credential_token (AES-256-GCM bound to providerID)
func (s *Server) resolveCredentials(r *http.Request, providerID, bodyToken string, bodyCreds providers.Credentials) (providers.Credentials, error) {
	creds := providers.Credentials{}
	mergeCreds(creds, s.cfg.ProviderDefaultCreds(providerID))

	if domain := strings.TrimSpace(r.Header.Get("X-Provider-Domain")); domain != "" {
		creds["domain"] = domain
	}

	hasPlainHeaders := strings.TrimSpace(r.Header.Get("X-Provider-Credentials")) != "" || hasPrefixedHeader(r.Header, "X-Provider-Cred-")
	if hasPlainHeaders {
		if !s.cfg.AllowPlainCredHeaders {
			return nil, errors.New("明文凭据请求头已禁用，请先通过 /login 或 /seal 获取加密令牌并通过 X-Provider-Token 传递")
		}
		if raw := strings.TrimSpace(r.Header.Get("X-Provider-Credentials")); raw != "" {
			parsed, err := decodeHeaderCredentials(raw)
			if err != nil {
				return nil, err
			}
			mergeCreds(creds, parsed)
		}
		const credHeaderPrefix = "X-Provider-Cred-"
		for headerKey, values := range r.Header {
			if len(values) == 0 || !strings.HasPrefix(strings.ToLower(headerKey), strings.ToLower(credHeaderPrefix)) {
				continue
			}
			credKey := strings.ToLower(strings.ReplaceAll(headerKey[len(credHeaderPrefix):], "-", "_"))
			if credKey != "" && strings.TrimSpace(values[0]) != "" {
				creds[credKey] = strings.TrimSpace(values[0])
			}
		}
	}

	if hdrToken := strings.TrimSpace(r.Header.Get("X-Provider-Token")); hdrToken != "" {
		opened, err := secret.Open(s.cfg.Secret, providerID, hdrToken)
		if err != nil {
			return nil, errors.New("X-Provider-Token 无效或与当前书源不匹配")
		}
		mergeCreds(creds, opened)
	}

	mergeCreds(creds, bodyCreds)

	if bodyToken = strings.TrimSpace(bodyToken); bodyToken != "" {
		opened, err := secret.Open(s.cfg.Secret, providerID, bodyToken)
		if err != nil {
			return nil, errors.New("credential_token 无效或与当前书源不匹配")
		}
		mergeCreds(creds, opened)
	}

	return creds, nil
}

func mergeCreds(dst providers.Credentials, src map[string]string) {
	for k, v := range src {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && strings.TrimSpace(v) != "" {
			dst[k] = strings.TrimSpace(v)
		}
	}
}

func hasPrefixedHeader(h http.Header, prefix string) bool {
	lowerPrefix := strings.ToLower(prefix)
	for k, vals := range h {
		if len(vals) > 0 && strings.TrimSpace(vals[0]) != "" && strings.HasPrefix(strings.ToLower(k), lowerPrefix) {
			return true
		}
	}
	return false
}

func decodeHeaderCredentials(raw string) (map[string]string, error) {
	data := []byte(raw)
	if !strings.HasPrefix(raw, "{") {
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(raw)
			if err != nil {
				return nil, errors.New("X-Provider-Credentials 必须是 JSON 或 base64 编码的 JSON")
			}
		}
		data = decoded
	}
	var out map[string]string
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, errors.New("X-Provider-Credentials 不是合法的 JSON 对象")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// JSON & error helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any, allowEmpty bool) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "读取请求体失败")
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		if allowEmpty {
			return true
		}
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体不能为空")
		return false
	}
	if err := json.Unmarshal(body, out); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体不是合法 JSON")
		return false
	}
	return true
}

func mapProviderError(w http.ResponseWriter, err error) {
	status, code, msg := classifyProviderError(err)
	writeError(w, status, code, msg)
}

func classifyProviderError(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, providers.ErrInvalidInput):
		return http.StatusBadRequest, "INVALID_INPUT", err.Error()
	case errors.Is(err, providers.ErrUnsupported):
		return http.StatusBadRequest, "UNSUPPORTED", err.Error()
	case errors.Is(err, providers.ErrNotConfigured):
		return http.StatusConflict, "PROVIDER_NOT_CONFIGURED", "缺少书源凭据或配置：" + err.Error()
	case errors.Is(err, providers.ErrSessionExpired):
		return http.StatusUnauthorized, "SESSION_EXPIRED", "书源会话已失效，请重新登录或更新凭据"
	case errors.Is(err, providers.ErrQuotaExceeded):
		return http.StatusForbidden, "QUOTA_EXCEEDED", "下载额度已用尽"
	case errors.Is(err, providers.ErrBlocked):
		return http.StatusServiceUnavailable, "PROVIDER_BLOCKED", "镜像被反爬拦截，请稍后重试或更换域名"
	case errors.Is(err, providers.ErrProviderUnavailable):
		return http.StatusBadGateway, "PROVIDER_UNAVAILABLE", "书源暂不可用：" + err.Error()
	case errors.Is(err, providers.ErrNotFound):
		return http.StatusNotFound, "NOT_FOUND", "未找到该图书"
	default:
		return http.StatusBadGateway, "PROVIDER_ERROR", err.Error()
	}
}
