// Package standardebooks implements providers.Provider for Standard Ebooks
// (https://standardebooks.org) using only Go standard library packages.
package standardebooks

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/imlinhanchao/ebook-api/internal/providers"
)

const (
	providerID    = "standard_ebooks"
	providerName  = "Standard Ebooks"
	defaultDomain = "https://standardebooks.org"

	httpUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	apiTimeout         = 30 * time.Second
	probeTimeout       = 10 * time.Second
	maxHTMLBytes       = 8 << 20
	maxDownloadRetries = 3
	maxFilenameRunes   = 180
)

var (
	downloadRetryBase = 300 * time.Millisecond

	reBookItem        = regexp.MustCompile(`(?is)<li\b[^>]*\btypeof=["']schema:Book["'][^>]*>(.*?)</li>`)
	reAboutAttr       = regexp.MustCompile(`(?is)\babout=["']([^"']+)["']`)
	reSchemaNameInURL = regexp.MustCompile(`(?is)<a\b[^>]*\bproperty=["']schema:url["'][^>]*>.*?<span\b[^>]*\bproperty=["']schema:name["'][^>]*>(.*?)</span>`)
	reSchemaNameSpan  = regexp.MustCompile(`(?is)<span\b[^>]*\bproperty=["']schema:name["'][^>]*>(.*?)</span>`)
	reAuthorPara      = regexp.MustCompile(`(?is)<p\b[^>]*\bclass=["'][^"']*\bauthor\b[^"']*["'][^>]*>(.*?)</p>`)
	reAuthorProp      = regexp.MustCompile(`(?is)\bproperty=["']schema:author["'][^>]*>(.*?)</(?:a|span|p)>`)
	rePictureBlock    = regexp.MustCompile(`(?is)<picture\b[^>]*>(.*?)</picture>`)
	reImgSrc          = regexp.MustCompile(`(?is)<img\b[^>]*\bsrc=["']([^"']+)["']`)
	reNextPage        = regexp.MustCompile(`(?is)<a\b[^>]*\brel=["']next["']`)
	reH1Title         = regexp.MustCompile(`(?is)<h1\b[^>]*>(.*?)</h1>`)
	reMetaOgTitle     = regexp.MustCompile(`(?is)<meta\b[^>]*\bproperty=["']og:title["'][^>]*\bcontent=["']([^"']+)["']`)
	reMetaOgImage     = regexp.MustCompile(`(?is)<meta\b[^>]*\bproperty=["']og:image["'][^>]*\bcontent=["']([^"']+)["']`)
	reDescDiv         = regexp.MustCompile(`(?is)<(?:div|section)\b[^>]*\b(?:property=["']schema:description["']|id=["']description["'])[^>]*>(.*?)</(?:div|section)>`)
	reAnchorTag       = regexp.MustCompile(`(?is)<a\b([^>]+)>`)
	reHrefAttr        = regexp.MustCompile(`(?is)\bhref=["']([^"']+)["']`)
	reClassAttr       = regexp.MustCompile(`(?is)\bclass=["']([^"']+)["']`)
	rePropAttr        = regexp.MustCompile(`(?is)\bproperty=["']([^"']+)["']`)
	reBreakTag        = regexp.MustCompile(`(?is)<(?:br|/p)\b[^>]*>`)
	reAnyTag          = regexp.MustCompile(`(?is)<[^>]+>`)
	reMultiSpace      = regexp.MustCompile(`[ \t\r\f]+`)
	reMultiNewline    = regexp.MustCompile(`\n{3,}`)
)

// Provider implements providers.Provider, providers.InfoProvider, and
// providers.DomainProber for Standard Ebooks.
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

// New creates a Standard Ebooks provider. domain defaults to
// https://standardebooks.org when empty; maxDownloadBytes <= 0 disables cap.
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
					return errors.New("standardebooks: too many redirects")
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
		Description:   "精排英文公版名著（全人工校对排版、现代封面、免登录直下 EPUB）",
		Capabilities:  p.Capabilities(),
		AuthRequired:  false,
		DefaultDomain: p.domain,
		Domains:       []string{p.domain},
	}
}

// ProbeDomain checks whether /ebooks returns HTTP 200.
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
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, target+"/ebooks", nil)
	if err != nil {
		return false
	}
	setHTMLHeaders(req, "")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode == http.StatusOK
}

// Search queries Standard Ebooks /ebooks search HTML page.
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
		"query": {keyword},
		"page":  {strconv.Itoa(page)},
	}
	if sortVal := strings.TrimSpace(q.Order); sortVal != "" {
		params.Set("sort", sortVal)
	}

	body, err := p.getHTML(ctx, origin+"/ebooks?"+params.Encode(), origin+"/")
	if err != nil {
		return providers.SearchResult{}, err
	}

	matches := reBookItem.FindAllStringSubmatch(body, -1)
	books := make([]providers.Book, 0, len(matches))
	for _, m := range matches {
		itemHTML := m[0]
		about := extractFirstGroup(reAboutAttr, itemHTML)
		ref := encodeRef(about)
		if ref == "" {
			continue
		}

		title := extractTitleFromCard(itemHTML)
		if title == "" {
			continue
		}
		authors := extractAuthorsFromCard(itemHTML)
		cover := extractCoverFromHTML(origin, itemHTML)
		sourcePath := decodeBookPathUnchecked(about, ref)

		books = append(books, providers.Book{
			Provider:  providerID,
			Ref:       ref,
			Title:     title,
			Authors:   authors,
			Language:  "English",
			Format:    "epub",
			CoverURL:  cover,
			SourceURL: origin + sourcePath,
		})
	}

	totalPages := page
	if reNextPage.MatchString(body) {
		totalPages = page + 1
	}

	return providers.SearchResult{
		Books:      books,
		Page:       page,
		TotalPages: totalPages,
	}, nil
}

// GetBook fetches a Standard Ebooks work page and extracts detail metadata.
func (p *Provider) GetBook(ctx context.Context, creds providers.Credentials, ref string) (providers.Book, error) {
	bookPath, canonicalRef, err := parseBookRef(ref)
	if err != nil {
		return providers.Book{}, err
	}
	origin := p.resolveOrigin(creds)
	pageHTML, err := p.getHTML(ctx, origin+bookPath, origin+"/ebooks")
	if err != nil {
		return providers.Book{}, err
	}

	title := cleanText(extractFirstGroup(reH1Title, pageHTML))
	if title == "" {
		title = strings.TrimSpace(html.UnescapeString(extractFirstGroup(reMetaOgTitle, pageHTML)))
	}
	authors := extractAuthorsFromDetail(pageHTML)
	cover := extractCoverFromHTML(origin, pageHTML)
	if cover == "" {
		cover = absolutize(origin, strings.TrimSpace(html.UnescapeString(extractFirstGroup(reMetaOgImage, pageHTML))))
	}
	description := extractDescription(pageHTML)

	return providers.Book{
		Provider:    providerID,
		Ref:         canonicalRef,
		Title:       title,
		Authors:     authors,
		Language:    "English",
		Format:      "epub",
		CoverURL:    cover,
		Description: description,
		SourceURL:   origin + bookPath,
	}, nil
}

// OpenDownload resolves the EPUB link on the book page, appends ?source=download
// to bypass the HTML interstitial, and streams the EPUB archive.
func (p *Provider) OpenDownload(ctx context.Context, creds providers.Credentials, ref string) (*providers.DownloadStream, error) {
	bookPath, canonicalRef, err := parseBookRef(ref)
	if err != nil {
		return nil, err
	}
	origin := p.resolveOrigin(creds)
	pageURL := origin + bookPath
	pageHTML, err := p.getHTML(ctx, pageURL, origin+"/ebooks")
	if err != nil {
		return nil, err
	}

	dlURL, ok := findEPUBDownloadURL(origin, pageHTML)
	if !ok {
		return nil, fmt.Errorf("%w: no EPUB download link found on %s", providers.ErrNotFound, bookPath)
	}

	resp, err := p.openWithRetry(ctx, dlURL, pageURL)
	if err != nil {
		return nil, err
	}
	if p.maxDownloadBytes > 0 && resp.ContentLength > p.maxDownloadBytes {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("standardebooks: file exceeds %d byte limit", p.maxDownloadBytes)
	}

	title := cleanText(extractFirstGroup(reH1Title, pageHTML))
	filename := buildFilename(title, canonicalRef)
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

func (p *Provider) getHTML(ctx context.Context, endpoint, referer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("standardebooks: build request: %w", err)
	}
	setHTMLHeaders(req, referer)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%w: %v", providers.ErrProviderUnavailable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTMLBytes))
	if err != nil {
		return "", fmt.Errorf("%w: read response: %v", providers.ErrProviderUnavailable, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("%w: page not found", providers.ErrNotFound)
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", fmt.Errorf("%w: HTTP 429", providers.ErrQuotaExceeded)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusServiceUnavailable:
		return "", fmt.Errorf("%w: HTTP %d", providers.ErrBlocked, resp.StatusCode)
	case resp.StatusCode >= http.StatusInternalServerError:
		return "", fmt.Errorf("%w: HTTP %d", providers.ErrProviderUnavailable, resp.StatusCode)
	case resp.StatusCode >= http.StatusBadRequest:
		return "", fmt.Errorf("standardebooks: unexpected HTTP %d", resp.StatusCode)
	}
	return string(body), nil
}

func (p *Provider) openWithRetry(ctx context.Context, rawURL, referer string) (*http.Response, error) {
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
			return nil, fmt.Errorf("standardebooks: build download request: %w", err)
		}
		req.Header.Set("User-Agent", httpUserAgent)
		req.Header.Set("Accept", "*/*")
		if referer != "" {
			req.Header.Set("Referer", referer)
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

		if status == http.StatusNotFound {
			return nil, fmt.Errorf("%w: epub file not found", providers.ErrNotFound)
		}
		if status == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%w: HTTP 429", providers.ErrQuotaExceeded)
		}
		if status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout {
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

func setHTMLHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", httpUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

// encodeRef converts an about path like "/ebooks/jane-austen/pride-and-prejudice"
// into a URL-safe colon-separated ref "jane-austen:pride-and-prejudice".
func encodeRef(aboutPath string) string {
	trimmed := strings.TrimSpace(aboutPath)
	trimmed = strings.TrimPrefix(trimmed, "/ebooks/")
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" || strings.Contains(trimmed, "..") {
		return ""
	}
	return strings.ReplaceAll(trimmed, "/", ":")
}

func parseBookRef(ref string) (bookPath, canonicalRef string, err error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" || strings.Contains(trimmed, "..") || strings.ContainsAny(trimmed, "\\?#") {
		return "", "", fmt.Errorf("%w: invalid standard_ebooks reference", providers.ErrInvalidInput)
	}
	if strings.HasPrefix(trimmed, "/") {
		canonical := encodeRef(trimmed)
		if canonical == "" {
			return "", "", fmt.Errorf("%w: invalid standard_ebooks reference", providers.ErrInvalidInput)
		}
		if !strings.HasPrefix(trimmed, "/ebooks/") {
			return "/ebooks/" + strings.TrimLeft(trimmed, "/"), canonical, nil
		}
		return trimmed, canonical, nil
	}
	slug := strings.ReplaceAll(strings.Trim(trimmed, ":"), ":", "/")
	if slug == "" {
		return "", "", fmt.Errorf("%w: invalid standard_ebooks reference", providers.ErrInvalidInput)
	}
	return "/ebooks/" + slug, encodeRef("/ebooks/" + slug), nil
}

func decodeBookPathUnchecked(about, ref string) string {
	about = strings.TrimSpace(about)
	if strings.HasPrefix(about, "/") {
		return about
	}
	return "/ebooks/" + strings.ReplaceAll(ref, ":", "/")
}

func findEPUBDownloadURL(origin, pageHTML string) (string, bool) {
	anchors := reAnchorTag.FindAllStringSubmatch(pageHTML, -1)
	var fallback string
	for _, a := range anchors {
		attrs := a[1]
		href := strings.TrimSpace(html.UnescapeString(extractFirstGroup(reHrefAttr, attrs)))
		if href == "" {
			continue
		}
		lowerHref := strings.ToLower(href)
		if !strings.Contains(lowerHref, ".epub") {
			continue
		}
		classVal := strings.ToLower(extractFirstGroup(reClassAttr, attrs))
		propVal := strings.ToLower(extractFirstGroup(rePropAttr, attrs))

		isPrimary := containsWord(classVal, "epub") || propVal == "schema:contenturl"
		if isPrimary {
			return ensureSourceDownload(absolutize(origin, href)), true
		}
		if fallback == "" {
			fallback = href
		}
	}
	if fallback != "" {
		return ensureSourceDownload(absolutize(origin, fallback)), true
	}
	return "", false
}

func ensureSourceDownload(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := parsed.Query()
	if q.Get("source") == "" {
		q.Set("source", "download")
		parsed.RawQuery = q.Encode()
	}
	return parsed.String()
}

func containsWord(haystack, word string) bool {
	for _, token := range strings.Fields(haystack) {
		if token == word {
			return true
		}
	}
	return false
}

func extractTitleFromCard(cardHTML string) string {
	if t := cleanText(extractFirstGroup(reSchemaNameInURL, cardHTML)); t != "" {
		return t
	}
	return cleanText(extractFirstGroup(reSchemaNameSpan, cardHTML))
}

func extractAuthorsFromCard(cardHTML string) []string {
	authorBlock := extractFirstGroup(reAuthorPara, cardHTML)
	if authorBlock == "" {
		return nil
	}
	return extractAuthorsFromBlock(authorBlock)
}

func extractAuthorsFromDetail(pageHTML string) []string {
	if block := extractFirstGroup(reAuthorPara, pageHTML); block != "" {
		if list := extractAuthorsFromBlock(block); len(list) > 0 {
			return list
		}
	}
	matches := reAuthorProp.FindAllStringSubmatch(pageHTML, -1)
	out := make([]string, 0, len(matches))
	seen := make(map[string]struct{})
	for _, m := range matches {
		name := cleanText(m[1])
		if name != "" {
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				out = append(out, name)
			}
		}
	}
	return out
}

func extractAuthorsFromBlock(block string) []string {
	spans := reSchemaNameSpan.FindAllStringSubmatch(block, -1)
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		if name := cleanText(s[1]); name != "" {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		if fallback := cleanText(block); fallback != "" {
			out = append(out, fallback)
		}
	}
	return out
}

func extractCoverFromHTML(origin, rawHTML string) string {
	if pic := extractFirstGroup(rePictureBlock, rawHTML); pic != "" {
		if src := strings.TrimSpace(html.UnescapeString(extractFirstGroup(reImgSrc, pic))); src != "" {
			return absolutize(origin, src)
		}
	}
	if src := strings.TrimSpace(html.UnescapeString(extractFirstGroup(reImgSrc, rawHTML))); src != "" {
		return absolutize(origin, src)
	}
	return ""
}

func extractDescription(pageHTML string) string {
	block := extractFirstGroup(reDescDiv, pageHTML)
	if block == "" {
		return ""
	}
	text := reBreakTag.ReplaceAllString(block, "\n\n")
	text = reAnyTag.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	lines := strings.Split(text, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(reMultiSpace.ReplaceAllString(line, " "))
		if line != "" {
			cleaned = append(cleaned, line)
		}
	}
	joined := strings.Join(cleaned, "\n\n")
	return reMultiNewline.ReplaceAllString(joined, "\n\n")
}

func extractFirstGroup(re *regexp.Regexp, input string) string {
	m := re.FindStringSubmatch(input)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func cleanText(raw string) string {
	text := reAnyTag.ReplaceAllString(raw, " ")
	text = html.UnescapeString(text)
	text = strings.ReplaceAll(text, "\n", " ")
	return strings.TrimSpace(reMultiSpace.ReplaceAllString(text, " "))
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

func buildFilename(title, canonicalRef string) string {
	stem := sanitizeTitle(title)
	if stem == "" {
		stem = sanitizeTitle(strings.ReplaceAll(canonicalRef, ":", "_"))
	}
	if stem == "" {
		stem = "standard_ebook"
	}
	name := stem + ".epub"
	runes := []rune(name)
	if len(runes) > maxFilenameRunes {
		ext := []rune(filepath.Ext(name))
		keep := maxFilenameRunes - len(ext)
		if keep > 0 {
			name = strings.TrimRight(string(runes[:keep]), " ._-") + string(ext)
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
			return n, fmt.Errorf("standardebooks: download exceeded %d byte limit", l.maxBytes)
		}
	}
	return n, err
}

func (l *limitedReadCloser) Close() error {
	return l.rc.Close()
}
