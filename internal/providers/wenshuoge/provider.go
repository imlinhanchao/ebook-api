// Package wenshuoge implements providers.Provider and optional auth/quota
// interfaces for Wenshuoge (https://www.wenshuoge.com) using Go stdlib only.
package wenshuoge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/imlinhanchao/ebook-api/internal/providers"
)

const (
	providerID    = "wenshuoge"
	providerName  = "文硕阁"
	defaultDomain = "https://www.wenshuoge.com"

	httpUserAgent = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36"

	apiTimeout          = 30 * time.Second
	probeTimeout        = 10 * time.Second
	defaultPollInterval = 3 * time.Second
	defaultPollAttempts = 40
	maxAPIBytes         = 16 << 20
	maxDownloadRetries  = 3
	maxCursorKeys       = 256
	maxFilenameRunes    = 180

	id24Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

var (
	downloadRetryBase = 300 * time.Millisecond

	reLDJsonScript = regexp.MustCompile(`(?is)<script\b[^>]*\btype=["']application/ld\+json["'][^>]*>(.*?)</script>`)
	reHTMLTitle    = regexp.MustCompile(`(?is)<h1\b[^>]*>(.*?)</h1>`)
	reAnyTag       = regexp.MustCompile(`(?is)<[^>]+>`)
)

// Provider implements providers.Provider, providers.InfoProvider,
// providers.Verifier, providers.PasswordAuthenticator, and
// providers.DomainProber for Wenshuoge.
type Provider struct {
	domain           string
	maxDownloadBytes int64
	httpClient       *http.Client
	downloadClient   *http.Client
	pollInterval     time.Duration
	maxPollAttempts  int

	cursorMu    sync.Mutex
	cursorCache map[string]map[int]string
}

var (
	_ providers.Provider              = (*Provider)(nil)
	_ providers.InfoProvider          = (*Provider)(nil)
	_ providers.Verifier              = (*Provider)(nil)
	_ providers.PasswordAuthenticator = (*Provider)(nil)
	_ providers.DomainProber          = (*Provider)(nil)
)

// New creates a Wenshuoge provider. domain defaults to
// https://www.wenshuoge.com when empty; maxDownloadBytes <= 0 disables cap.
func New(domain string, maxDownloadBytes int64) *Provider {
	normalized := normalizeDomain(domain)
	if normalized == "" {
		normalized = defaultDomain
	}
	return &Provider{
		domain:           normalized,
		maxDownloadBytes: maxDownloadBytes,
		httpClient:       &http.Client{Timeout: apiTimeout},
		downloadClient: &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("wenshuoge: too many redirects")
				}
				if len(via) > 0 && !sameOrigin(req.URL, via[0].URL) {
					req.Header.Del("Cookie")
				}
				return nil
			},
		},
		pollInterval:    defaultPollInterval,
		maxPollAttempts: defaultPollAttempts,
		cursorCache:     make(map[string]map[int]string),
	}
}

func (p *Provider) ID() string          { return providerID }
func (p *Provider) DisplayName() string { return providerName }

func (p *Provider) Capabilities() []string {
	return []string{"search", "detail", "download", "login", "cookies", "quota", "probe"}
}

// Info exposes metadata and credential requirements to /v1/providers.
func (p *Provider) Info() providers.ProviderInfo {
	return providers.ProviderInfo{
		ID:             providerID,
		Name:           providerName,
		Description:    "中文公版典籍数字书阁（1.34 万+经史子集与近现代经典；搜书免登，下载 EPUB 需免费账号）",
		Capabilities:   p.Capabilities(),
		AuthRequired:   false,
		AuthModes:      []string{"password", "cookies"},
		DefaultDomain:  p.domain,
		Domains:        []string{p.domain},
		CredentialKeys: []string{"domain", "wenshuoge_session"},
	}
}

// ProbeDomain requests /api/v2/health without credentials.
func (p *Provider) ProbeDomain(ctx context.Context, domain string) bool {
	target := normalizeDomain(domain)
	if target == "" {
		return false
	}
	probeCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(ctx, probeTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, target+"/api/v2/health", nil)
	if err != nil {
		return false
	}
	setAPIHeaders(req, target)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode == http.StatusOK
}

// Login exchanges email + password + CSRF token/cookie for a reusable
// wenshuoge_session cookie credential.
func (p *Provider) Login(ctx context.Context, domain, email, password string) (providers.Credentials, error) {
	origin := normalizeDomain(domain)
	if origin == "" {
		origin = p.domain
	}
	email = strings.TrimSpace(email)
	if email == "" || password == "" {
		return nil, fmt.Errorf("%w: email and password are required", providers.ErrInvalidInput)
	}

	csrfToken, csrfCookie, err := p.fetchCSRF(ctx, origin, "")
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(map[string]string{
		"email":    email,
		"password": password,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/v2/auth/password/login", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("wenshuoge: build login request: %w", err)
	}
	setAPIHeaders(req, origin)
	req.Header.Set("Content-Type", "application/json")
	if csrfToken != "" {
		req.Header.Set("X-CSRF-Token", csrfToken)
	}
	if csrfCookie != "" {
		req.AddCookie(&http.Cookie{Name: "wenshuoge_csrf", Value: csrfCookie})
	}

	body, cookies, err := p.doAPI(ctx, req, true)
	if err != nil {
		return nil, err
	}
	session := strings.TrimSpace(cookies["wenshuoge_session"])
	if session == "" {
		return nil, fmt.Errorf("wenshuoge: login response did not set wenshuoge_session cookie (body=%d bytes)", len(body))
	}

	return providers.Credentials{
		"domain":            origin,
		"wenshuoge_session": session,
	}, nil
}

// Verify validates wenshuoge_session via /api/v2/downloads/quota (checked
// first because /auth/status returns 200 even when anonymous) and /auth/status.
func (p *Provider) Verify(ctx context.Context, creds providers.Credentials) (providers.AccountInfo, error) {
	origin := p.resolveOrigin(creds)
	session := sessionFromCreds(creds)
	if session == "" {
		return providers.AccountInfo{}, fmt.Errorf("%w: wenshuoge_session is required", providers.ErrNotConfigured)
	}

	quotaReq, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/v2/downloads/quota", nil)
	if err != nil {
		return providers.AccountInfo{}, err
	}
	setAPIHeaders(quotaReq, origin)
	quotaReq.AddCookie(&http.Cookie{Name: "wenshuoge_session", Value: session})

	quotaBody, _, err := p.doAPI(ctx, quotaReq, false)
	if err != nil {
		return providers.AccountInfo{}, err
	}
	var quotaResp struct {
		Data struct {
			Used      int `json:"used"`
			Limit     int `json:"limit"`
			Remaining int `json:"remaining"`
		} `json:"data"`
	}
	if err := json.Unmarshal(quotaBody, &quotaResp); err != nil {
		return providers.AccountInfo{}, fmt.Errorf("wenshuoge: invalid quota JSON: %w", err)
	}

	info := providers.AccountInfo{
		DailyUsed:      quotaResp.Data.Used,
		DailyLimit:     quotaResp.Data.Limit,
		DailyRemaining: quotaResp.Data.Remaining,
		QuotaKnown:     true,
	}

	statusReq, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/v2/auth/status", nil)
	if err == nil {
		setAPIHeaders(statusReq, origin)
		statusReq.AddCookie(&http.Cookie{Name: "wenshuoge_session", Value: session})
		if statusBody, _, sErr := p.doAPI(ctx, statusReq, false); sErr == nil {
			var statusResp struct {
				Data struct {
					User struct {
						Email string `json:"email"`
					} `json:"user"`
				} `json:"data"`
			}
			if json.Unmarshal(statusBody, &statusResp) == nil {
				info.Email = strings.TrimSpace(statusResp.Data.User.Email)
			}
		}
	}

	return info, nil
}

// Search queries /api/v2/catalog anonymously and caches keyset cursors per
// query so page > 1 navigation works seamlessly.
func (p *Provider) Search(ctx context.Context, creds providers.Credentials, q providers.SearchQuery) (providers.SearchResult, error) {
	keyword := strings.TrimSpace(q.Q)
	if keyword == "" {
		return providers.SearchResult{}, fmt.Errorf("%w: search query is empty", providers.ErrInvalidInput)
	}
	page := q.Page
	if page < 1 {
		page = 1
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	origin := p.resolveOrigin(creds)
	cacheKey := fmt.Sprintf("%s|%s|%d", origin, keyword, limit)
	params := url.Values{
		"q":     {keyword},
		"limit": {strconv.Itoa(limit)},
	}
	if page > 1 {
		cursor, ok := p.getCursor(cacheKey, page)
		if !ok || cursor == "" {
			return providers.SearchResult{
				Books:      []providers.Book{},
				Page:       page,
				TotalPages: page,
			}, nil
		}
		params.Set("cursor", cursor)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/v2/catalog?"+params.Encode(), nil)
	if err != nil {
		return providers.SearchResult{}, fmt.Errorf("wenshuoge: build search request: %w", err)
	}
	setAPIHeaders(req, origin)

	raw, _, err := p.doAPI(ctx, req, false)
	if err != nil {
		return providers.SearchResult{}, err
	}

	var payload struct {
		Data struct {
			Books []catalogBook `json:"books"`
		} `json:"data"`
		Meta struct {
			Total      int    `json:"total"`
			NextCursor string `json:"nextCursor"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return providers.SearchResult{}, fmt.Errorf("wenshuoge: invalid search JSON: %w", err)
	}

	books := make([]providers.Book, 0, len(payload.Data.Books))
	for _, item := range payload.Data.Books {
		books = append(books, mapCatalogBook(origin, item))
	}

	if nextCursor := strings.TrimSpace(payload.Meta.NextCursor); nextCursor != "" {
		p.putCursor(cacheKey, page+1, nextCursor)
	}

	totalPages := 1
	if payload.Meta.Total > 0 {
		totalPages = (payload.Meta.Total + limit - 1) / limit
	}
	if strings.TrimSpace(payload.Meta.NextCursor) != "" && totalPages <= page {
		totalPages = page + 1
	}
	if totalPages < page {
		totalPages = page
	}

	return providers.SearchResult{
		Books:      books,
		Page:       page,
		TotalPages: totalPages,
	}, nil
}

// GetBook fetches /book/<ref> HTML and extracts schema.org JSON-LD metadata.
func (p *Provider) GetBook(ctx context.Context, creds providers.Credentials, ref string) (providers.Book, error) {
	cleanedRef, err := cleanRef(ref)
	if err != nil {
		return providers.Book{}, err
	}
	origin := p.resolveOrigin(creds)
	bookURL := origin + "/book/" + url.PathEscape(cleanedRef)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bookURL, nil)
	if err != nil {
		return providers.Book{}, err
	}
	req.Header.Set("User-Agent", httpUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Referer", origin+"/")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return providers.Book{}, ctx.Err()
		}
		return providers.Book{}, fmt.Errorf("%w: %v", providers.ErrProviderUnavailable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes))
	if err != nil {
		return providers.Book{}, fmt.Errorf("%w: read detail: %v", providers.ErrProviderUnavailable, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return providers.Book{}, fmt.Errorf("%w: book not found", providers.ErrNotFound)
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return providers.Book{}, fmt.Errorf("%w: HTTP %d", providers.ErrProviderUnavailable, resp.StatusCode)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return providers.Book{}, fmt.Errorf("wenshuoge: detail returned HTTP %d", resp.StatusCode)
	}

	htmlText := string(body)
	book := providers.Book{
		Provider:  providerID,
		Ref:       cleanedRef,
		Format:    "epub",
		SourceURL: bookURL,
	}

	if ld, ok := extractJSONLD(htmlText); ok {
		if name, _ := ld["name"].(string); strings.TrimSpace(name) != "" {
			book.Title = strings.TrimSpace(name)
		}
		if desc, _ := ld["description"].(string); strings.TrimSpace(desc) != "" {
			book.Description = strings.TrimSpace(desc)
		}
		if img, _ := ld["image"].(string); strings.TrimSpace(img) != "" {
			book.CoverURL = absolutize(origin, img)
		}
		if authors := extractLDAuthors(ld["author"]); len(authors) > 0 {
			book.Authors = authors
		}
	}
	if book.Title == "" {
		if m := reHTMLTitle.FindStringSubmatch(htmlText); len(m) >= 2 {
			t := strings.TrimSpace(html.UnescapeString(reAnyTag.ReplaceAllString(m[1], "")))
			if t != "" {
				book.Title = t
			}
		}
	}
	return book, nil
}

// OpenDownload obtains a CSRF token, authorizes an EPUB export job, polls
// until ready, and opens the streaming EPUB response with same-origin cookie
// protection.
func (p *Provider) OpenDownload(ctx context.Context, creds providers.Credentials, ref string) (*providers.DownloadStream, error) {
	cleanedRef, err := cleanRef(ref)
	if err != nil {
		return nil, err
	}
	origin := p.resolveOrigin(creds)
	session := sessionFromCreds(creds)
	if session == "" {
		return nil, fmt.Errorf("%w: wenshuoge_session is required for downloads", providers.ErrNotConfigured)
	}

	csrfToken, csrfCookie, err := p.fetchCSRF(ctx, origin, session)
	if err != nil {
		return nil, err
	}

	authPayload, err := json.Marshal(map[string]string{
		"editionId": cleanedRef,
		"visitorId": randomID24(),
		"requestId": randomUUIDv4(),
	})
	if err != nil {
		return nil, err
	}

	authReq, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/v2/downloads/epub/authorize", bytes.NewReader(authPayload))
	if err != nil {
		return nil, err
	}
	setAPIHeaders(authReq, origin)
	authReq.Header.Set("Content-Type", "application/json")
	if csrfToken != "" {
		authReq.Header.Set("X-CSRF-Token", csrfToken)
	}
	authReq.AddCookie(&http.Cookie{Name: "wenshuoge_session", Value: session})
	if csrfCookie != "" {
		authReq.AddCookie(&http.Cookie{Name: "wenshuoge_csrf", Value: csrfCookie})
	}

	authBody, _, err := p.doAPI(ctx, authReq, false)
	if err != nil {
		return nil, err
	}

	var authResp struct {
		Data struct {
			State   string `json:"state"`
			Intent  string `json:"intent"`
			URL     string `json:"url"`
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(authBody, &authResp); err != nil {
		return nil, fmt.Errorf("wenshuoge: invalid authorize JSON: %w", err)
	}

	if strings.EqualFold(authResp.Data.State, "failed") {
		msg := strings.TrimSpace(authResp.Data.Message)
		if msg == "" {
			msg = "文硕阁生成电子书失败"
		}
		return nil, fmt.Errorf("wenshuoge: %s", msg)
	}

	fileURL := strings.TrimSpace(authResp.Data.URL)
	if !strings.EqualFold(authResp.Data.State, "ready") || fileURL == "" {
		intent := strings.TrimSpace(authResp.Data.Intent)
		if intent == "" {
			return nil, fmt.Errorf("wenshuoge: authorize did not return an intent or download url")
		}
		fileURL, err = p.pollDownloadReady(ctx, origin, session, intent)
		if err != nil {
			return nil, err
		}
	}

	resolvedURL := absolutize(origin, fileURL)
	if resolvedURL == "" {
		return nil, fmt.Errorf("%w: invalid download URL returned by wenshuoge", providers.ErrNotFound)
	}

	referer := origin + "/book/" + url.PathEscape(cleanedRef)
	resp, err := p.openFileWithRetry(ctx, origin, resolvedURL, referer, session)
	if err != nil {
		return nil, err
	}
	if p.maxDownloadBytes > 0 && resp.ContentLength > p.maxDownloadBytes {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("wenshuoge: file exceeds %d byte limit", p.maxDownloadBytes)
	}

	filename := sanitizeFilename(filenameFromResponse(resp))
	if filename == "" {
		filename = "wenshuoge_" + sanitizeFilename(cleanedRef) + ".epub"
	}
	if filename == "" {
		filename = "book.epub"
	}

	size := resp.ContentLength
	if size <= 0 {
		size = -1
	}

	return &providers.DownloadStream{
		Body: &limitedReadCloser{
			rc:       resp.Body,
			maxBytes: p.maxDownloadBytes,
		},
		Filename:    filename,
		Format:      "epub",
		ContentType: "application/epub+zip",
		Size:        size,
	}, nil
}

func (p *Provider) pollDownloadReady(ctx context.Context, origin, session, intent string) (string, error) {
	statusEndpoint := origin + "/api/v2/downloads/epub/status?intent=" + url.QueryEscape(intent)
	interval := p.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	attempts := p.maxPollAttempts
	if attempts <= 0 {
		attempts = defaultPollAttempts
	}

	for i := 0; i < attempts; i++ {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusEndpoint, nil)
		if err != nil {
			return "", err
		}
		setAPIHeaders(req, origin)
		req.AddCookie(&http.Cookie{Name: "wenshuoge_session", Value: session})

		body, _, err := p.doAPI(ctx, req, false)
		if err != nil {
			return "", err
		}

		var st struct {
			Data struct {
				State   string `json:"state"`
				URL     string `json:"url"`
				Message string `json:"message"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &st); err != nil {
			return "", fmt.Errorf("wenshuoge: invalid status JSON: %w", err)
		}

		if strings.EqualFold(st.Data.State, "failed") {
			msg := strings.TrimSpace(st.Data.Message)
			if msg == "" {
				msg = "生成电子书失败"
			}
			return "", fmt.Errorf("wenshuoge: %s", msg)
		}
		if strings.EqualFold(st.Data.State, "ready") && strings.TrimSpace(st.Data.URL) != "" {
			return strings.TrimSpace(st.Data.URL), nil
		}
	}
	return "", fmt.Errorf("%w: timed out waiting for EPUB generation", providers.ErrProviderUnavailable)
}

func (p *Provider) fetchCSRF(ctx context.Context, origin, session string) (token, cookieVal string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/v2/auth/csrf", nil)
	if err != nil {
		return "", "", err
	}
	setAPIHeaders(req, origin)
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "wenshuoge_session", Value: session})
	}

	body, cookies, err := p.doAPI(ctx, req, false)
	if err != nil {
		return "", "", err
	}
	var payload struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &payload)
	return strings.TrimSpace(payload.Data.Token), strings.TrimSpace(cookies["wenshuoge_csrf"]), nil
}

func (p *Provider) doAPI(ctx context.Context, req *http.Request, isLogin bool) ([]byte, map[string]string, error) {
	resp, err := p.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("%w: %v", providers.ErrProviderUnavailable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: read response: %v", providers.ErrProviderUnavailable, err)
	}
	cookies := cookiesToMap(resp.Cookies())

	status := resp.StatusCode
	if isHTMLChallenge(status, resp.Header, body) {
		return nil, cookies, fmt.Errorf("%w: HTTP %d", providers.ErrBlocked, status)
	}
	if status >= http.StatusInternalServerError {
		return nil, cookies, fmt.Errorf("%w: HTTP %d", providers.ErrProviderUnavailable, status)
	}
	if status >= http.StatusBadRequest {
		return nil, cookies, classifyAPIError(status, body, isLogin)
	}
	return body, cookies, nil
}

func (p *Provider) openFileWithRetry(ctx context.Context, origin, rawURL, referer, session string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxDownloadRetries; attempt++ {
		if attempt > 0 {
			delay := downloadRetryBase << (attempt - 1)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", httpUserAgent)
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Accept-Encoding", "identity")
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		if sameOriginString(rawURL, origin) && session != "" {
			req.AddCookie(&http.Cookie{Name: "wenshuoge_session", Value: session})
		}

		resp, err := p.downloadClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("%w: %v", providers.ErrProviderUnavailable, err)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		status := resp.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<10))
		_ = resp.Body.Close()

		switch status {
		case http.StatusUnauthorized, http.StatusForbidden:
			if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
				return nil, fmt.Errorf("%w: HTTP %d", providers.ErrBlocked, status)
			}
			return nil, fmt.Errorf("%w: HTTP %d", providers.ErrSessionExpired, status)
		case http.StatusNotFound:
			return nil, fmt.Errorf("%w: HTTP 404", providers.ErrNotFound)
		case http.StatusTooManyRequests:
			return nil, fmt.Errorf("%w: HTTP 429", providers.ErrQuotaExceeded)
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			lastErr = fmt.Errorf("%w: download HTTP %d", providers.ErrProviderUnavailable, status)
			continue
		default:
			return nil, fmt.Errorf("%w: download HTTP %d", providers.ErrProviderUnavailable, status)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: download failed", providers.ErrProviderUnavailable)
	}
	return nil, lastErr
}

func classifyAPIError(status int, body []byte, isLogin bool) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(bytes.TrimSpace(body), &env)
	code := strings.ToLower(strings.TrimSpace(env.Error.Code))
	msg := strings.TrimSpace(env.Error.Message)
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", status)
	}

	if isLogin {
		return fmt.Errorf("%w: %s", providers.ErrInvalidInput, msg)
	}

	if status == http.StatusTooManyRequests || strings.Contains(code, "quota") || strings.Contains(code, "download_daily_limit") {
		return fmt.Errorf("%w: %s", providers.ErrQuotaExceeded, msg)
	}
	if status == http.StatusUnauthorized || strings.Contains(code, "login_required") || strings.Contains(code, "unauthorized") || strings.Contains(code, "session") {
		return fmt.Errorf("%w: %s", providers.ErrSessionExpired, msg)
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("%w: %s", providers.ErrNotFound, msg)
	}
	if status == http.StatusForbidden {
		return fmt.Errorf("%w: %s", providers.ErrSessionExpired, msg)
	}
	return fmt.Errorf("wenshuoge: %s", msg)
}

func isHTMLChallenge(status int, header http.Header, body []byte) bool {
	if status != http.StatusForbidden && status != http.StatusServiceUnavailable {
		return false
	}
	if ct := strings.ToLower(header.Get("Content-Type")); strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml") {
		return true
	}
	return bytes.HasPrefix(bytes.TrimSpace(body), []byte("<"))
}

func (p *Provider) resolveOrigin(creds providers.Credentials) string {
	if creds != nil {
		if explicit := normalizeDomain(creds["domain"]); explicit != "" {
			return explicit
		}
	}
	return p.domain
}

func (p *Provider) getCursor(key string, page int) (string, bool) {
	p.cursorMu.Lock()
	defer p.cursorMu.Unlock()
	pages := p.cursorCache[key]
	if pages == nil {
		return "", false
	}
	cur, ok := pages[page]
	return cur, ok
}

func (p *Provider) putCursor(key string, page int, cursor string) {
	p.cursorMu.Lock()
	defer p.cursorMu.Unlock()
	if len(p.cursorCache) >= maxCursorKeys {
		for k := range p.cursorCache {
			delete(p.cursorCache, k)
			if len(p.cursorCache) < maxCursorKeys {
				break
			}
		}
	}
	pages := p.cursorCache[key]
	if pages == nil {
		pages = make(map[int]string)
		p.cursorCache[key] = pages
	}
	pages[page] = cursor
}

type catalogBook struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Author       string   `json:"author"`
	Era          string   `json:"era"`
	Category     string   `json:"category"`
	VariantLabel string   `json:"variantLabel"`
	Tags         []string `json:"tags"`
}

func mapCatalogBook(origin string, item catalogBook) providers.Book {
	id := strings.TrimSpace(item.ID)
	tags := make([]string, 0, len(item.Tags))
	for _, t := range item.Tags {
		if trimmed := strings.TrimSpace(t); trimmed != "" {
			tags = append(tags, trimmed)
		}
	}
	return providers.Book{
		Provider:    providerID,
		Ref:         id,
		Title:       strings.TrimSpace(item.Title),
		Authors:     splitAuthors(item.Author),
		Year:        strings.TrimSpace(item.Era),
		Publisher:   strings.TrimSpace(item.Category),
		Language:    strings.TrimSpace(item.VariantLabel),
		Format:      "epub",
		Description: strings.Join(tags, " · "),
		SourceURL:   origin + "/book/" + url.PathEscape(id),
	}
}

func splitAuthors(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	normalized := strings.NewReplacer("、", ",", "，", ",", ";", ",").Replace(raw)
	parts := strings.Split(normalized, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return []string{raw}
	}
	return out
}

func extractJSONLD(htmlText string) (map[string]any, bool) {
	matches := reLDJsonScript.FindAllStringSubmatch(htmlText, -1)
	for _, m := range matches {
		raw := strings.TrimSpace(m[1])
		if raw == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err == nil && len(obj) > 0 {
			return obj, true
		}
	}
	return nil, false
}

func extractLDAuthors(raw any) []string {
	switch v := raw.(type) {
	case string:
		return splitAuthors(v)
	case map[string]any:
		if name, _ := v["name"].(string); strings.TrimSpace(name) != "" {
			return splitAuthors(name)
		}
	case []any:
		var out []string
		for _, item := range v {
			out = append(out, extractLDAuthors(item)...)
		}
		return out
	}
	return nil
}

func setAPIHeaders(req *http.Request, origin string) {
	req.Header.Set("User-Agent", httpUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
}

func cookiesToMap(list []*http.Cookie) map[string]string {
	out := make(map[string]string, len(list))
	for _, c := range list {
		if c != nil && strings.TrimSpace(c.Name) != "" {
			out[c.Name] = c.Value
		}
	}
	return out
}

func sessionFromCreds(creds providers.Credentials) string {
	if creds == nil {
		return ""
	}
	return strings.TrimSpace(creds["wenshuoge_session"])
}

func cleanRef(ref string) (string, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" || strings.Contains(trimmed, "..") || strings.ContainsAny(trimmed, "/\\?#") {
		return "", fmt.Errorf("%w: invalid wenshuoge book reference", providers.ErrInvalidInput)
	}
	return trimmed, nil
}

func normalizeDomain(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func absolutize(origin, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "data:") {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if parsed.IsAbs() {
		return parsed.String()
	}
	base, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return base.ResolveReference(parsed).String()
}

func sameOriginString(rawURL, domain string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	b, err := url.Parse(domain)
	if err != nil {
		return false
	}
	return sameOrigin(u, b)
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func filenameFromResponse(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	disposition := strings.TrimSpace(resp.Header.Get("Content-Disposition"))
	if disposition == "" {
		return ""
	}
	if _, params, err := mime.ParseMediaType(disposition); err == nil {
		if name := strings.TrimSpace(params["filename"]); name != "" {
			return name
		}
	}
	return ""
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	name = path.Base(name)
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if out == "" || out == "." || out == ".." || out == "/" {
		return ""
	}
	runes := []rune(out)
	if len(runes) > maxFilenameRunes {
		ext := []rune(filepath.Ext(out))
		keep := maxFilenameRunes - len(ext)
		if keep > 0 {
			out = strings.TrimRight(string(runes[:keep]), " .") + string(ext)
		}
	}
	return out
}

func randomUUIDv4() string {
	var b [16]byte
	_, _ = io.ReadFull(rand.Reader, b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hx := hex.EncodeToString(b[:])
	return hx[:8] + "-" + hx[8:12] + "-" + hx[12:16] + "-" + hx[16:20] + "-" + hx[20:]
}

func randomID24() string {
	var b [24]byte
	_, _ = io.ReadFull(rand.Reader, b[:])
	out := make([]byte, 24)
	for i, v := range b {
		out[i] = id24Alphabet[int(v)%len(id24Alphabet)]
	}
	return string(out)
}

type limitedReadCloser struct {
	rc       io.ReadCloser
	read     int64
	maxBytes int64
}

func (l *limitedReadCloser) Read(p []byte) (int, error) {
	n, err := l.rc.Read(p)
	if n > 0 {
		l.read += int64(n)
		if l.maxBytes > 0 && l.read > l.maxBytes {
			return n, fmt.Errorf("wenshuoge: download exceeded %d byte limit", l.maxBytes)
		}
	}
	return n, err
}

func (l *limitedReadCloser) Close() error {
	return l.rc.Close()
}
