package api

import (
	"net/http"
	"strings"

	"github.com/imlinhanchao/ebook-api/internal/providers"
	"github.com/imlinhanchao/ebook-api/internal/secret"
)

type providerView struct {
	providers.ProviderInfo
	DefaultConfigured bool `json:"default_configured"`
}

func (s *Server) buildProviderView(p providers.Provider) providerView {
	info := providers.Describe(p)
	defaults := s.cfg.ProviderDefaultCreds(info.ID)
	return providerView{
		ProviderInfo:      info,
		DefaultConfigured: len(defaults) > 0,
	}
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	list := s.registry.List()
	out := make([]providerView, 0, len(list))
	for _, p := range list {
		out = append(out, s.buildProviderView(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"default_provider": s.cfg.DefaultProvider,
		"providers":        out,
	})
}

func (s *Server) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := s.registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "PROVIDER_NOT_FOUND", "未知书源")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": s.buildProviderView(p)})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := s.registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "PROVIDER_NOT_FOUND", "未知书源")
		return
	}
	auth, ok := p.(providers.PasswordAuthenticator)
	if !ok {
		writeError(w, http.StatusBadRequest, "UNSUPPORTED", "该书源不支持密码登录")
		return
	}

	var body struct {
		Domain   string `json:"domain"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body, false) {
		return
	}
	if strings.TrimSpace(body.Email) == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "email 和 password 不能为空")
		return
	}

	creds, err := auth.Login(r.Context(), strings.TrimSpace(body.Domain), body.Email, body.Password)
	if err != nil {
		mapProviderError(w, err)
		return
	}

	token, err := secret.Seal(s.cfg.Secret, id, creds)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "加密凭据失败")
		return
	}

	resp := map[string]any{
		"provider":         id,
		"credential_token": token,
	}
	if verifier, ok := p.(providers.Verifier); ok {
		if info, err := verifier.Verify(r.Context(), creds); err == nil {
			resp["account"] = info
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSeal encrypts caller-supplied raw credentials (e.g. cookies) into an
// opaque AES-256-GCM credential_token for subsequent requests.
func (s *Server) handleSeal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := s.registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "PROVIDER_NOT_FOUND", "未知书源")
		return
	}

	var body struct {
		Credentials providers.Credentials `json:"credentials"`
		Verify      *bool                 `json:"verify"`
	}
	if !decodeJSON(w, r, &body, false) {
		return
	}
	cleaned := providers.Credentials{}
	mergeCreds(cleaned, body.Credentials)
	if len(cleaned) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "credentials 不能为空")
		return
	}

	shouldVerify := true
	if body.Verify != nil {
		shouldVerify = *body.Verify
	}

	resp := map[string]any{"provider": id}
	if shouldVerify {
		if verifier, ok := p.(providers.Verifier); ok {
			account, err := verifier.Verify(r.Context(), cleaned)
			if err != nil {
				mapProviderError(w, err)
				return
			}
			resp["account"] = account
		}
	}

	token, err := secret.Seal(s.cfg.Secret, id, cleaned)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "加密凭据失败")
		return
	}
	resp["credential_token"] = token
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := s.registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "PROVIDER_NOT_FOUND", "未知书源")
		return
	}
	verifier, ok := p.(providers.Verifier)
	if !ok {
		writeError(w, http.StatusBadRequest, "UNSUPPORTED", "该书源不支持账号校验")
		return
	}

	var body struct {
		CredentialToken string                `json:"credential_token"`
		Credentials     providers.Credentials `json:"credentials"`
	}
	if !decodeJSON(w, r, &body, true) {
		return
	}

	creds, err := s.resolveCredentials(r, id, body.CredentialToken, body.Credentials)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", err.Error())
		return
	}

	account, err := verifier.Verify(r.Context(), creds)
	if err != nil {
		mapProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": id,
		"account":  account,
	})
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := s.registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "PROVIDER_NOT_FOUND", "未知书源")
		return
	}
	prober, ok := p.(providers.DomainProber)
	if !ok {
		writeError(w, http.StatusBadRequest, "UNSUPPORTED", "该书源不支持域名探测")
		return
	}

	var body struct {
		Domain string `json:"domain"`
	}
	if !decodeJSON(w, r, &body, true) {
		return
	}
	domain := strings.TrimSpace(body.Domain)
	if domain == "" {
		domain = providers.Describe(p).DefaultDomain
	}
	if domain == "" {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT", "缺少 domain 参数")
		return
	}

	healthy := prober.ProbeDomain(r.Context(), domain)
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": id,
		"domain":   domain,
		"healthy":  healthy,
	})
}
