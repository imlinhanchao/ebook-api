package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/imlinhanchao/ebook-api/internal/config"
	"github.com/imlinhanchao/ebook-api/internal/providers"
	"github.com/imlinhanchao/ebook-api/internal/secret"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := secret.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return k
}

type fakeProvider struct {
	id           string
	name         string
	searchFn     func(ctx context.Context, creds providers.Credentials, q providers.SearchQuery) (providers.SearchResult, error)
	getBookFn    func(ctx context.Context, creds providers.Credentials, ref string) (providers.Book, error)
	openDownFn   func(ctx context.Context, creds providers.Credentials, ref string) (*providers.DownloadStream, error)
	verifyFn     func(ctx context.Context, creds providers.Credentials) (providers.AccountInfo, error)
	loginFn      func(ctx context.Context, domain, email, password string) (providers.Credentials, error)
	probeFn      func(ctx context.Context, domain string) bool
	defaultDom   string
	authRequired bool
}

func (f *fakeProvider) ID() string             { return f.id }
func (f *fakeProvider) DisplayName() string    { return f.name }
func (f *fakeProvider) Capabilities() []string { return []string{"search", "detail", "download"} }
func (f *fakeProvider) Info() providers.ProviderInfo {
	return providers.ProviderInfo{
		ID:            f.id,
		Name:          f.name,
		Capabilities:  f.Capabilities(),
		AuthRequired:  f.authRequired,
		DefaultDomain: f.defaultDom,
	}
}
func (f *fakeProvider) Search(ctx context.Context, creds providers.Credentials, q providers.SearchQuery) (providers.SearchResult, error) {
	return f.searchFn(ctx, creds, q)
}
func (f *fakeProvider) GetBook(ctx context.Context, creds providers.Credentials, ref string) (providers.Book, error) {
	return f.getBookFn(ctx, creds, ref)
}
func (f *fakeProvider) OpenDownload(ctx context.Context, creds providers.Credentials, ref string) (*providers.DownloadStream, error) {
	return f.openDownFn(ctx, creds, ref)
}
func (f *fakeProvider) Verify(ctx context.Context, creds providers.Credentials) (providers.AccountInfo, error) {
	return f.verifyFn(ctx, creds)
}
func (f *fakeProvider) Login(ctx context.Context, domain, email, password string) (providers.Credentials, error) {
	return f.loginFn(ctx, domain, email, password)
}
func (f *fakeProvider) ProbeDomain(ctx context.Context, domain string) bool {
	return f.probeFn(ctx, domain)
}

func TestAPIAuthAndHealth(t *testing.T) {
	reg := providers.NewRegistry()
	reg.Register(&fakeProvider{id: "zlib", name: "Z-Library"})
	cfg := &config.Config{APIKey: "secret-key", Secret: testKey(t), DefaultProvider: "zlib"}
	srv := New(cfg, reg)

	// Health is public
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/health status = %d", rec.Code)
	}

	// Protected endpoint without key -> 401
	req = httptest.NewRequest(http.MethodGet, "/v1/providers", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without API key, got %d", rec.Code)
	}

	// Protected endpoint with Bearer key -> 200
	req = httptest.NewRequest(http.MethodGet, "/v1/providers", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with Bearer key, got %d", rec.Code)
	}

	// Protected endpoint with X-API-Key -> 200
	req = httptest.NewRequest(http.MethodGet, "/v1/providers", nil)
	req.Header.Set("X-API-Key", "secret-key")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with X-API-Key, got %d", rec.Code)
	}
}

func TestEncryptedTokenFlowAndPlainHeaderRejection(t *testing.T) {
	key := testKey(t)
	var seenCreds providers.Credentials
	reg := providers.NewRegistry()
	reg.Register(&fakeProvider{
		id:   "zlib",
		name: "Z-Library",
		searchFn: func(ctx context.Context, creds providers.Credentials, q providers.SearchQuery) (providers.SearchResult, error) {
			seenCreds = creds
			return providers.SearchResult{
				Books: []providers.Book{{Provider: "zlib", Ref: "1:abc", Title: "三体"}},
				Page:  q.Page, TotalPages: 2,
			}, nil
		},
		verifyFn: func(ctx context.Context, creds providers.Credentials) (providers.AccountInfo, error) {
			return providers.AccountInfo{Email: "u@example.com", DailyLimit: 10, DailyUsed: 1, DailyRemaining: 9, QuotaKnown: true}, nil
		},
	})
	reg.Register(&fakeProvider{
		id:   "other",
		name: "Other",
		searchFn: func(ctx context.Context, creds providers.Credentials, q providers.SearchQuery) (providers.SearchResult, error) {
			return providers.SearchResult{}, nil
		},
	})

	cfg := &config.Config{
		Secret:          key,
		DefaultProvider: "zlib",
		DefaultCreds: map[string]providers.Credentials{
			"zlib": {"domain": "https://default.example"},
		},
	}
	srv := New(cfg, reg)

	// 1. Plaintext credential headers must be rejected by default
	req := httptest.NewRequest(http.MethodGet, "/v1/search?q=test", nil)
	req.Header.Set("X-Provider-Cred-Remix-Userkey", "plain-secret")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when plaintext header is disabled, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Seal credentials via /seal -> get opaque credential_token
	sealBody := `{"credentials":{"remix_userid":"42","remix_userkey":"my-secret-key"}}`
	req = httptest.NewRequest(http.MethodPost, "/v1/providers/zlib/seal", strings.NewReader(sealBody))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("seal failed: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "my-secret-key") {
		t.Fatalf("seal response leaked raw credential")
	}
	var sealResp struct {
		CredentialToken string                `json:"credential_token"`
		Account         providers.AccountInfo `json:"account"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sealResp); err != nil {
		t.Fatalf("decode seal response: %v", err)
	}
	if !strings.HasPrefix(sealResp.CredentialToken, "ebk1.") || sealResp.Account.DailyRemaining != 9 {
		t.Fatalf("unexpected seal response: %+v", sealResp)
	}

	// 3. Use X-Provider-Token on GET /v1/search
	req = httptest.NewRequest(http.MethodGet, "/v1/search?q=%E4%B8%89%E4%BD%93", nil)
	req.Header.Set("X-Provider-Token", sealResp.CredentialToken)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("search with X-Provider-Token failed: %d %s", rec.Code, rec.Body.String())
	}
	if seenCreds["domain"] != "https://default.example" || seenCreds["remix_userid"] != "42" || seenCreds["remix_userkey"] != "my-secret-key" {
		t.Fatalf("decrypted creds mismatch: %+v", seenCreds)
	}

	// 4. Cross-provider replay with zlib token against "other" must fail
	req = httptest.NewRequest(http.MethodGet, "/v1/search?q=test&provider=other", nil)
	req.Header.Set("X-Provider-Token", sealResp.CredentialToken)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on cross-provider token replay, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMultiProviderFanoutSearch(t *testing.T) {
	reg := providers.NewRegistry()
	reg.Register(&fakeProvider{
		id:   "zlib",
		name: "Z-Library",
		searchFn: func(ctx context.Context, creds providers.Credentials, q providers.SearchQuery) (providers.SearchResult, error) {
			return providers.SearchResult{
				Books: []providers.Book{{Provider: "zlib", Ref: "1:a", Title: "Book A"}},
				Page:  1, TotalPages: 1,
			}, nil
		},
	})
	reg.Register(&fakeProvider{
		id:   "gutenberg",
		name: "Project Gutenberg",
		searchFn: func(ctx context.Context, creds providers.Credentials, q providers.SearchQuery) (providers.SearchResult, error) {
			return providers.SearchResult{}, providers.ErrProviderUnavailable
		},
	})

	srv := New(&config.Config{Secret: testKey(t), DefaultProvider: "zlib"}, reg)
	req := httptest.NewRequest(http.MethodGet, "/v1/search?q=test&provider=all", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("multi-search status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Books   []providers.Book       `json:"books"`
		Results []providerSearchBucket `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Books) != 1 || resp.Books[0].Title != "Book A" {
		t.Fatalf("unexpected merged books: %+v", resp.Books)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 provider buckets, got %d", len(resp.Results))
	}
	if resp.Results[0].Provider != "gutenberg" || resp.Results[0].Error == nil || resp.Results[0].Error.Code != "PROVIDER_UNAVAILABLE" {
		t.Fatalf("expected gutenberg bucket error, got %+v", resp.Results[0])
	}
}

func TestDownloadStreaming(t *testing.T) {
	reg := providers.NewRegistry()
	reg.Register(&fakeProvider{
		id:   "zlib",
		name: "Z-Library",
		openDownFn: func(ctx context.Context, creds providers.Credentials, ref string) (*providers.DownloadStream, error) {
			if ref != "42:hash" {
				t.Fatalf("unexpected ref %q", ref)
			}
			content := []byte("hello-epub")
			return &providers.DownloadStream{
				Body:        io.NopCloser(bytes.NewReader(content)),
				Filename:    "三体.epub",
				Format:      "epub",
				ContentType: "application/epub+zip",
				Size:        int64(len(content)),
			}, nil
		},
	})

	srv := New(&config.Config{Secret: testKey(t), DefaultProvider: "zlib"}, reg)
	req := httptest.NewRequest(http.MethodGet, "/v1/providers/zlib/books/42:hash/download", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello-epub" {
		t.Fatalf("unexpected download body: %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/epub+zip" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "filename*=UTF-8''") {
		t.Fatalf("Content-Disposition missing RFC 5987 filename*: %q", cd)
	}
	if fmtHdr := rec.Header().Get("X-Book-Format"); fmtHdr != "epub" {
		t.Fatalf("X-Book-Format = %q", fmtHdr)
	}
}

func TestLoginVerifyProbeWithToken(t *testing.T) {
	reg := providers.NewRegistry()
	reg.Register(&fakeProvider{
		id:         "zlib",
		name:       "Z-Library",
		defaultDom: "https://z-lib.gd",
		loginFn: func(ctx context.Context, domain, email, password string) (providers.Credentials, error) {
			return providers.Credentials{"domain": "https://z-lib.gd", "remix_userid": "7", "remix_userkey": "raw-key"}, nil
		},
		verifyFn: func(ctx context.Context, creds providers.Credentials) (providers.AccountInfo, error) {
			if creds["remix_userkey"] != "raw-key" {
				return providers.AccountInfo{}, providers.ErrSessionExpired
			}
			return providers.AccountInfo{Email: "u@example.com", DailyLimit: 10, DailyUsed: 2, DailyRemaining: 8, QuotaKnown: true}, nil
		},
		probeFn: func(ctx context.Context, domain string) bool {
			return domain == "https://z-lib.gd"
		},
	})

	srv := New(&config.Config{Secret: testKey(t), DefaultProvider: "zlib"}, reg)

	// Login -> returns encrypted credential_token, NOT raw-key
	req := httptest.NewRequest(http.MethodPost, "/v1/providers/zlib/login", strings.NewReader(`{"email":"u@example.com","password":"pw"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "raw-key") {
		t.Fatalf("login response leaked raw remix_userkey")
	}
	var loginResp struct {
		CredentialToken string `json:"credential_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &loginResp); err != nil || !strings.HasPrefix(loginResp.CredentialToken, "ebk1.") {
		t.Fatalf("unexpected login response: %s", rec.Body.String())
	}

	// Verify using the encrypted token
	req = httptest.NewRequest(http.MethodPost, "/v1/providers/zlib/verify", strings.NewReader(`{"credential_token":"`+loginResp.CredentialToken+`"}`))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"daily_remaining":8`) {
		t.Fatalf("verify failed: %d %s", rec.Code, rec.Body.String())
	}

	// Probe
	req = httptest.NewRequest(http.MethodPost, "/v1/providers/zlib/probe", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"healthy":true`) {
		t.Fatalf("probe failed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDownloadCDNCacheHeadersAndPublicDownload(t *testing.T) {
	openCalls := 0
	reg := providers.NewRegistry()
	reg.Register(&fakeProvider{
		id:   "zlib",
		name: "Z-Library",
		openDownFn: func(ctx context.Context, creds providers.Credentials, ref string) (*providers.DownloadStream, error) {
			openCalls++
			content := []byte("cdn-book-bytes")
			return &providers.DownloadStream{
				Body:        io.NopCloser(bytes.NewReader(content)),
				Filename:    "book.epub",
				Format:      "epub",
				ContentType: "application/epub+zip",
				Size:        int64(len(content)),
			}, nil
		},
	})

	srv := New(&config.Config{
		APIKey:         "secret-key",
		Secret:         testKey(t),
		PublicDownload: true,
	}, reg)

	// /v1/providers still requires API key
	req := httptest.NewRequest(http.MethodGet, "/v1/providers", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on /v1/providers without key, got %d", rec.Code)
	}

	// GET download works without API key when PublicDownload=true
	req = httptest.NewRequest(http.MethodGet, "/v1/providers/zlib/books/99:hash99/download", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on public download, got %d", rec.Code)
	}
	cc := rec.Header().Get("Cache-Control")
	etag := rec.Header().Get("ETag")
	if !strings.Contains(cc, "public") || !strings.Contains(cc, "immutable") {
		t.Fatalf("unexpected Cache-Control: %q", cc)
	}
	if etag == "" {
		t.Fatalf("expected non-empty ETag")
	}
	if openCalls != 1 {
		t.Fatalf("expected 1 OpenDownload call, got %d", openCalls)
	}

	// Conditional GET with matching ETag returns 304 without calling provider
	req = httptest.NewRequest(http.MethodGet, "/v1/providers/zlib/books/99:hash99/download", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified, got %d", rec.Code)
	}
	if openCalls != 1 {
		t.Fatalf("expected OpenDownload not to be called on 304, got %d", openCalls)
	}
}

func TestStaticEmbeddedFallback(t *testing.T) {
	reg := providers.NewRegistry()
	srv := New(&config.Config{
		Secret:    testKey(t),
		StaticDir: "/nonexistent/static/dir",
	}, reg)

	// Root path should fall back to embedded deployment homepage (without /console links)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	rootBody := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(rootBody, "Ebook API") {
		t.Fatalf("GET / fallback failed: %d", rec.Code)
	}
	if strings.Contains(rootBody, "/console") || strings.Contains(rootBody, "console.html") {
		t.Fatalf("GET / homepage must not link or reference /console")
	}

	// /console and /console/ should serve the standalone tester UI
	for _, consolePath := range []string{"/console", "/console/"} {
		req = httptest.NewRequest(http.MethodGet, consolePath, nil)
		rec = httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		consoleBody := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(consoleBody, "Ebook API 测试台") || !strings.Contains(consoleBody, "/static/app.js") {
			t.Fatalf("GET %s fallback failed: %d", consolePath, rec.Code)
		}
	}

	// Embedded static assets should all be served
	for _, asset := range []struct {
		path    string
		snippet string
	}{
		{"/static/home.css", "--bg-canvas"},
		{"/static/home.js", "buildEnvConfig"},
		{"/static/app.js", "STORAGE_KEY"},
		{"/static/zlibrary-book-source.json", `"id": "zlib"`},
		{"/static/public-book-sources.json", `"id": "gutendex"`},
	} {
		req = httptest.NewRequest(http.MethodGet, asset.path, nil)
		rec = httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), asset.snippet) {
			t.Fatalf("GET %s fallback failed: %d", asset.path, rec.Code)
		}
	}

	// StaticDir="off" disables both homepage and console UI
	disabled := New(&config.Config{Secret: testKey(t), StaticDir: "off"}, reg)
	for _, path := range []string{"/", "/console"} {
		req = httptest.NewRequest(http.MethodGet, path, nil)
		rec = httptest.NewRecorder()
		disabled.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for %s when StaticDir=off, got %d", path, rec.Code)
		}
	}
}
