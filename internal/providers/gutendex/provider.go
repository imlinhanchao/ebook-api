// Package gutendex implements providers.Provider for Project Gutenberg via the
// public Gutendex JSON API using only Go standard library packages.
package gutendex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/imlinhanchao/ebook-api/internal/providers"
)

const (
	providerID    = "gutendex"
	providerName  = "古登堡公版书库"
	defaultDomain = "https://gutendex.com"

	defaultPageSize    = 32
	apiTimeout         = 30 * time.Second
	probeTimeout       = 10 * time.Second
	maxAPIResponseSize = 16 << 20
	maxDownloadRetries = 3
	maxFilenameRunes   = 180
)

var downloadRetryBase = 300 * time.Millisecond

var languageAliases = map[string]string{
	"chinese":  "zh",
	"english":  "en",
	"japanese": "ja",
	"german":   "de",
	"french":   "fr",
	"spanish":  "es",
	"russian":  "ru",
}

// Provider implements providers.Provider, providers.InfoProvider, and
// providers.DomainProber for Gutendex.
type Provider struct {
	domain           string
	maxDownloadBytes int64
	httpClient       *http.Client
	downloadClient   *http.Client
}

var (
	_ providers.Provider     = (*Provider)(nil)
	_ providers.InfoProvider = (*Provider)(nil)
	_ providers.DomainProber = (*Provider)(nil)
)

// New creates a Gutendex provider. domain defaults to https://gutendex.com when
// empty; maxDownloadBytes <= 0 disables the size cap.
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
					return errors.New("gutendex: too many redirects")
				}
				return nil
			},
		},
	}
}

func (p *Provider) ID() string          { return providerID }
func (p *Provider) DisplayName() string { return providerName }

func (p *Provider) Capabilities() []string {
	return []string{"search", "detail", "download", "probe"}
}

// Info exposes provider metadata to /v1/providers.
func (p *Provider) Info() providers.ProviderInfo {
	return providers.ProviderInfo{
		ID:            providerID,
		Name:          providerName,
		Description:   "Project Gutenberg 开源公版书库（7 万+多语种公版名著，免登录直连下载）",
		Capabilities:  p.Capabilities(),
		AuthRequired:  false,
		DefaultDomain: p.domain,
		Domains:       []string{p.domain},
	}
}

// ProbeDomain checks whether the mirror's /books endpoint responds with HTTP 200.
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
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, target+"/books?page=1", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode == http.StatusOK
}

// Search queries Gutendex's /books endpoint with optional extension, language,
// and sort filters.
func (p *Provider) Search(ctx context.Context, creds providers.Credentials, q providers.SearchQuery) (providers.SearchResult, error) {
	keyword := strings.TrimSpace(q.Q)
	if keyword == "" {
		return providers.SearchResult{}, fmt.Errorf("%w: search query is empty", providers.ErrInvalidInput)
	}
	page := q.Page
	if page < 1 {
		page = 1
	}

	origin := p.resolveOrigin(creds)
	params := url.Values{
		"search": {keyword},
		"page":   {strconv.Itoa(page)},
	}
	if mime := mapExtensionParam(q.Extensions); mime != "" {
		params.Set("mime_type", mime)
	}
	if langs := mapLanguagesParam(q.Languages); langs != "" {
		params.Set("languages", langs)
	}
	if sortVal := mapOrderParam(q.Order); sortVal != "" {
		params.Set("sort", sortVal)
	}

	raw, err := p.getJSON(ctx, origin+"/books?"+params.Encode())
	if err != nil {
		return providers.SearchResult{}, err
	}

	var payload struct {
		Count   int            `json:"count"`
		Next    string         `json:"next"`
		Results []gutendexBook `json:"results"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return providers.SearchResult{}, fmt.Errorf("gutendex: invalid search JSON: %w", err)
	}

	books := make([]providers.Book, 0, len(payload.Results))
	for _, item := range payload.Results {
		books = append(books, mapBook(origin, item))
	}

	totalPages := 1
	if payload.Count > 0 {
		totalPages = (payload.Count + defaultPageSize - 1) / defaultPageSize
	}
	if payload.Next != "" && totalPages <= page {
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

// GetBook fetches a single book record by numeric Gutenberg ID.
func (p *Provider) GetBook(ctx context.Context, creds providers.Credentials, ref string) (providers.Book, error) {
	id, err := cleanRef(ref)
	if err != nil {
		return providers.Book{}, err
	}
	origin := p.resolveOrigin(creds)
	item, err := p.fetchBook(ctx, origin, id)
	if err != nil {
		return providers.Book{}, err
	}
	return mapBook(origin, item), nil
}

// OpenDownload resolves the best EPUB/TXT download link from Gutendex and
// opens a streaming response with retry and size protection.
func (p *Provider) OpenDownload(ctx context.Context, creds providers.Credentials, ref string) (*providers.DownloadStream, error) {
	id, err := cleanRef(ref)
	if err != nil {
		return nil, err
	}
	origin := p.resolveOrigin(creds)
	item, err := p.fetchBook(ctx, origin, id)
	if err != nil {
		return nil, err
	}

	dlURL, format, contentType, ok := pickDownloadFormat(origin, item.Formats)
	if !ok {
		return nil, fmt.Errorf("%w: no epub or txt format available for %s", providers.ErrNotFound, id)
	}

	resp, err := p.openWithRetry(ctx, dlURL)
	if err != nil {
		return nil, err
	}
	if p.maxDownloadBytes > 0 && resp.ContentLength > p.maxDownloadBytes {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("gutendex: file exceeds %d byte limit", p.maxDownloadBytes)
	}

	if respCT := strings.TrimSpace(resp.Header.Get("Content-Type")); respCT != "" &&
		!strings.EqualFold(respCT, "application/octet-stream") {
		contentType = respCT
	}
	filename := buildDownloadFilename(item.Title, id, format)
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
		Format:      format,
		ContentType: contentType,
		Size:        size,
	}, nil
}

func (p *Provider) fetchBook(ctx context.Context, origin, ref string) (gutendexBook, error) {
	raw, err := p.getJSON(ctx, origin+"/books/"+url.PathEscape(ref))
	if err != nil {
		return gutendexBook{}, err
	}
	var item gutendexBook
	if err := json.Unmarshal(raw, &item); err != nil {
		return gutendexBook{}, fmt.Errorf("gutendex: invalid book JSON: %w", err)
	}
	if item.ID == 0 {
		if parsed, convErr := strconv.Atoi(ref); convErr == nil {
			item.ID = parsed
		}
	}
	return item, nil
}

func (p *Provider) getJSON(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("gutendex: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", providers.ErrProviderUnavailable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize))
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %v", providers.ErrProviderUnavailable, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: book not found", providers.ErrNotFound)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: rate limited by upstream", providers.ErrQuotaExceeded)
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return nil, fmt.Errorf("%w: HTTP %d", providers.ErrProviderUnavailable, resp.StatusCode)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("gutendex: unexpected HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func (p *Provider) openWithRetry(ctx context.Context, rawURL string) (*http.Response, error) {
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
			return nil, fmt.Errorf("gutendex: build download request: %w", err)
		}
		req.Header.Set("Accept", "*/*")

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

		if status == http.StatusNotFound {
			return nil, fmt.Errorf("%w: download URL returned 404", providers.ErrNotFound)
		}
		if status == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%w: download rate limited", providers.ErrQuotaExceeded)
		}
		if isRetryableStatus(status) {
			lastErr = fmt.Errorf("%w: download HTTP %d", providers.ErrProviderUnavailable, status)
			continue
		}
		return nil, fmt.Errorf("%w: download failed with HTTP %d", providers.ErrProviderUnavailable, status)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: download failed", providers.ErrProviderUnavailable)
	}
	return nil, lastErr
}

func (p *Provider) resolveOrigin(creds providers.Credentials) string {
	if creds != nil {
		if explicit := normalizeDomain(creds["domain"]); explicit != "" {
			return explicit
		}
	}
	return p.domain
}

type gutendexBook struct {
	ID      int               `json:"id"`
	Title   string            `json:"title"`
	Authors []gutendexAuthor  `json:"authors"`
	Languages []string        `json:"languages"`
	Subjects  []string        `json:"subjects"`
	Formats   map[string]string `json:"formats"`
}

type gutendexAuthor struct {
	Name string `json:"name"`
}

func mapBook(origin string, item gutendexBook) providers.Book {
	ref := strconv.Itoa(item.ID)
	authors := make([]string, 0, len(item.Authors))
	for _, a := range item.Authors {
		if name := strings.TrimSpace(a.Name); name != "" {
			authors = append(authors, name)
		}
	}

	_, format, _, _ := pickDownloadFormat(origin, item.Formats)
	cover := absolutize(origin, item.Formats["image/jpeg"])

	return providers.Book{
		Provider:    providerID,
		Ref:         ref,
		Title:       strings.TrimSpace(item.Title),
		Authors:     authors,
		Language:    strings.Join(item.Languages, ", "),
		Format:      format,
		CoverURL:    cover,
		Description: strings.Join(item.Subjects, " · "),
		SourceURL:   "https://www.gutenberg.org/ebooks/" + url.PathEscape(ref),
	}
}

func pickDownloadFormat(origin string, formats map[string]string) (downloadURL, format, contentType string, ok bool) {
	if len(formats) == 0 {
		return "", "", "", false
	}
	candidates := []struct {
		key         string
		format      string
		contentType string
	}{
		{"application/epub+zip", "epub", "application/epub+zip"},
		{"text/plain; charset=utf-8", "txt", "text/plain; charset=utf-8"},
		{"text/plain; charset=us-ascii", "txt", "text/plain; charset=utf-8"},
		{"text/plain", "txt", "text/plain; charset=utf-8"},
	}
	for _, c := range candidates {
		if raw := strings.TrimSpace(formats[c.key]); raw != "" {
			resolved := absolutize(origin, raw)
			if isHTTPURL(resolved) {
				return resolved, c.format, c.contentType, true
			}
		}
	}
	return "", "", "", false
}

func mapExtensionParam(exts []string) string {
	for _, raw := range exts {
		switch strings.ToLower(strings.TrimPrefix(strings.TrimSpace(raw), ".")) {
		case "epub":
			return "application/epub"
		case "txt":
			return "text/plain"
		}
	}
	return ""
}

func mapLanguagesParam(langs []string) string {
	out := make([]string, 0, len(langs))
	for _, raw := range langs {
		norm := strings.ToLower(strings.TrimSpace(raw))
		if norm == "" {
			continue
		}
		if mapped, ok := languageAliases[norm]; ok {
			out = append(out, mapped)
			continue
		}
		if len(norm) == 2 {
			out = append(out, norm)
		}
	}
	return strings.Join(out, ",")
}

func mapOrderParam(order string) string {
	switch strings.ToLower(strings.TrimSpace(order)) {
	case "ascending", "descending", "popular":
		return strings.ToLower(strings.TrimSpace(order))
	default:
		return ""
	}
}

func cleanRef(ref string) (string, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return "", fmt.Errorf("%w: book reference is required", providers.ErrInvalidInput)
	}
	if strings.ContainsAny(trimmed, "/\\?") {
		return "", fmt.Errorf("%w: invalid gutendex book reference", providers.ErrInvalidInput)
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
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if parsed.IsAbs() {
		if isHTTPURL(parsed.String()) {
			return parsed.String()
		}
		return ""
	}
	base, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return base.ResolveReference(parsed).String()
}

func isHTTPURL(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

func isRetryableStatus(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func buildDownloadFilename(title, id, format string) string {
	stem := sanitizeTitle(title)
	if stem == "" {
		stem = "gutenberg_" + id
	}
	ext := "." + format
	name := stem + ext
	runes := []rune(name)
	if len(runes) > maxFilenameRunes {
		extRunes := []rune(filepath.Ext(name))
		keep := maxFilenameRunes - len(extRunes)
		if keep > 0 {
			name = strings.TrimRight(string(runes[:keep]), " ._-") + string(extRunes)
		}
	}
	return name
}

func sanitizeTitle(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r < 0x20 || r == 0x7f:
			continue
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Trim(strings.TrimSpace(b.String()), ".")
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
			return n, fmt.Errorf("gutendex: download exceeded %d byte limit", l.maxBytes)
		}
	}
	return n, err
}

func (l *limitedReadCloser) Close() error {
	return l.rc.Close()
}
