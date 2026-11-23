// Package providers defines the pluggable book-source contract for ebook-api.
//
// Each book source (e.g. Z-Library, Anna's Archive, Project Gutenberg,
// Standard Ebooks) implements Provider and optionally one or more capability
// interfaces (InfoProvider, Verifier, PasswordAuthenticator, DomainProber,
// FileDownloader), then registers itself with a Registry at startup.
package providers

import (
	"context"
	"errors"
	"io"
)

// Credentials is a provider-specific bag of secrets (never logged, never
// leaked in error messages).
type Credentials map[string]string

// Clone returns a shallow copy of c, or an empty map if c is nil.
func (c Credentials) Clone() Credentials {
	out := make(Credentials, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// SearchQuery is a normalized search request shared across all providers.
type SearchQuery struct {
	Q          string   `json:"q"`
	Page       int      `json:"page,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	Extensions []string `json:"ext,omitempty"`
	Languages  []string `json:"lang,omitempty"`
	YearFrom   int      `json:"year_from,omitempty"`
	YearTo     int      `json:"year_to,omitempty"`
	Order      string   `json:"order,omitempty"`
}

// Book is a normalized search result / detail record.
type Book struct {
	Provider    string   `json:"provider"`
	Ref         string   `json:"ref"`
	Title       string   `json:"title"`
	Authors     []string `json:"authors"`
	Year        string   `json:"year,omitempty"`
	Publisher   string   `json:"publisher,omitempty"`
	Language    string   `json:"language,omitempty"`
	Format      string   `json:"format,omitempty"`
	Size        string   `json:"size,omitempty"`
	ISBN        string   `json:"isbn,omitempty"`
	CoverURL    string   `json:"cover_url,omitempty"`
	Description string   `json:"description,omitempty"`
	SourceURL   string   `json:"source_url,omitempty"`
}

// SearchResult is one page of search results from a single provider.
type SearchResult struct {
	Books      []Book `json:"books"`
	Page       int    `json:"page"`
	TotalPages int    `json:"total_pages"`
}

// AccountInfo is the non-secret account summary returned by Verifier.
type AccountInfo struct {
	Email          string `json:"email,omitempty"`
	DailyUsed      int    `json:"daily_used"`
	DailyLimit     int    `json:"daily_limit"`
	DailyRemaining int    `json:"daily_remaining"`
	Premium        bool   `json:"premium"`
	QuotaKnown     bool   `json:"quota_known"`
}

// DownloadStream represents an open book download stream ready to be piped to
// an HTTP response or saved to disk. Callers must close Body.
type DownloadStream struct {
	Body        io.ReadCloser
	Filename    string
	Format      string
	ContentType string
	Size        int64 // -1 when unknown
}

// DownloadResult describes a book saved to disk.
type DownloadResult struct {
	Path     string `json:"path"`
	Filename string `json:"filename"`
	Format   string `json:"format"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

// ProgressFunc reports bytes written / total bytes (total may be -1).
type ProgressFunc func(written, total int64)

// Provider is the minimal contract every book source must implement.
type Provider interface {
	ID() string
	DisplayName() string
	Capabilities() []string
	Search(ctx context.Context, creds Credentials, q SearchQuery) (SearchResult, error)
	GetBook(ctx context.Context, creds Credentials, ref string) (Book, error)
	OpenDownload(ctx context.Context, creds Credentials, ref string) (*DownloadStream, error)
}

// InfoProvider supplies extended metadata for /v1/providers.
type InfoProvider interface {
	Info() ProviderInfo
}

// Verifier is implemented by providers whose accounts/credentials can be
// verified (and optionally report download quota).
type Verifier interface {
	Verify(ctx context.Context, creds Credentials) (AccountInfo, error)
}

// PasswordAuthenticator is implemented by providers that support exchanging an
// email + password for reusable session credentials.
type PasswordAuthenticator interface {
	Login(ctx context.Context, domain, email, password string) (Credentials, error)
}

// DomainProber is implemented by providers that support health-checking mirror
// domains.
type DomainProber interface {
	ProbeDomain(ctx context.Context, domain string) bool
}

// FileDownloader is optionally implemented by providers that support saving a
// download directly to a directory with progress reporting and SHA256 hashing.
type FileDownloader interface {
	Download(ctx context.Context, creds Credentials, ref, destDir string, progress ProgressFunc) (DownloadResult, error)
}

// ProviderInfo describes a registered book source to API consumers.
type ProviderInfo struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Description    string   `json:"description,omitempty"`
	Capabilities   []string `json:"capabilities"`
	AuthRequired   bool     `json:"auth_required"`
	AuthModes      []string `json:"auth_modes,omitempty"`
	DefaultDomain  string   `json:"default_domain,omitempty"`
	Domains        []string `json:"domains,omitempty"`
	CredentialKeys []string `json:"credential_keys,omitempty"`
}

// Typed errors shared by all providers; the HTTP API maps them to status codes.
var (
	ErrNotConfigured       = errors.New("provider not configured")
	ErrSessionExpired      = errors.New("provider session expired")
	ErrBlocked             = errors.New("provider blocked by bot protection")
	ErrQuotaExceeded       = errors.New("download quota exceeded")
	ErrNotFound            = errors.New("not found")
	ErrProviderUnavailable = errors.New("provider unavailable")
	ErrUnsupported         = errors.New("unsupported")
	ErrInvalidInput        = errors.New("invalid input")
)
