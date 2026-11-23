package zlib

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imlinhanchao/ebook-api/internal/providers"
)

func TestOpenDownloadStreamsAndRetries(t *testing.T) {
	origRetry := downloadRetryBase
	downloadRetryBase = time.Millisecond
	t.Cleanup(func() { downloadRetryBase = origRetry })

	var attempts atomic.Int32
	var cdnServer *httptest.Server
	cdnServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("remix-userkey") != "" {
			t.Errorf("CDN request leaked credentials")
		}
		w.Header().Set("Content-Type", "application/epub+zip")
		w.Header().Set("Content-Disposition", `attachment; filename="test-book.epub"`)
		_, _ = w.Write([]byte("epub-binary-content"))
	}))
	defer cdnServer.Close()

	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/eapi/book/42/abc123/file" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":1,"file":{"downloadLink":"` + cdnServer.URL + `/signed/book.epub"}}`))
	}))
	defer mirror.Close()

	p := New([]string{mirror.URL}, 1024)
	stream, err := p.OpenDownload(context.Background(), providers.Credentials{
		"domain":        mirror.URL,
		"remix_userid":  "100",
		"remix_userkey": "secret",
	}, "42:abc123")
	if err != nil {
		t.Fatalf("OpenDownload failed: %v", err)
	}
	defer stream.Body.Close()

	if stream.Filename != "test-book.epub" {
		t.Fatalf("expected filename test-book.epub, got %q", stream.Filename)
	}
	if stream.Format != "epub" {
		t.Fatalf("expected format epub, got %q", stream.Format)
	}
	if stream.ContentType != "application/epub+zip" {
		t.Fatalf("expected Content-Type application/epub+zip, got %q", stream.ContentType)
	}
	data, err := io.ReadAll(stream.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if string(data) != "epub-binary-content" {
		t.Fatalf("unexpected body: %q", string(data))
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts (1 retry), got %d", attempts.Load())
	}
}

func TestOpenDownloadRespectsMaxBytes(t *testing.T) {
	cdnServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte(strings.Repeat("a", 64)))
	}))
	defer cdnServer.Close()

	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":1,"file":{"downloadLink":"` + cdnServer.URL + `/big.pdf"}}`))
	}))
	defer mirror.Close()

	p := New([]string{mirror.URL}, 16)
	_, err := p.OpenDownload(context.Background(), providers.Credentials{
		"domain":        mirror.URL,
		"remix_userid":  "100",
		"remix_userkey": "secret",
	}, "1:hash")
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected size limit error, got %v", err)
	}
}
