package standardebooks

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imlinhanchao/ebook-api/internal/providers"
)

func init() {
	downloadRetryBase = 1 * time.Millisecond
}

const sampleSearchHTML = `<!doctype html>
<html><body>
  <ol class="ebooks-list">
    <li typeof="schema:Book" about="/ebooks/jane-austen/pride-and-prejudice">
      <div class="thumbnail-container">
        <a property="schema:url" href="/ebooks/jane-austen/pride-and-prejudice">
          <picture><img src="/images/covers/jane-austen_pride.jpg" alt="cover"/></picture>
        </a>
      </div>
      <p><a property="schema:url" href="/ebooks/jane-austen/pride-and-prejudice"><span property="schema:name">Pride &amp; Prejudice</span></a></p>
      <p class="author"><a property="schema:author" href="/ebooks/jane-austen"><span property="schema:name">Jane Austen</span></a></p>
    </li>
  </ol>
  <nav><a rel="next" href="/ebooks?query=pride&amp;page=2">Next</a></nav>
</body></html>`

const sampleDetailHTML = `<!doctype html>
<html><body>
  <h1 property="schema:name">Pride &amp; Prejudice</h1>
  <p class="author"><a property="schema:author" href="/ebooks/jane-austen"><span property="schema:name">Jane Austen</span></a></p>
  <picture><img src="/images/covers/jane-austen_pride_large.jpg" alt="cover"/></picture>
  <div property="schema:description">
    <p>First paragraph of description.</p>
    <p>Second paragraph &amp; more.</p>
  </div>
  <section id="download">
    <a class="epub" property="schema:contentUrl" href="/downloads/jane-austen_pride-and-prejudice.epub">Compatible epub</a>
  </section>
</body></html>`

func TestStandardEbooksProvider(t *testing.T) {
	var sawSourceDownload bool
	var lastReferer string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ebooks":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleSearchHTML))
		case "/ebooks/jane-austen/pride-and-prejudice":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(sampleDetailHTML))
		case "/downloads/jane-austen_pride-and-prejudice.epub":
			sawSourceDownload = r.URL.Query().Get("source") == "download"
			lastReferer = r.Header.Get("Referer")
			w.Header().Set("Content-Type", "application/epub+zip")
			_, _ = w.Write([]byte("PK\x03\x04se-epub"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := New(srv.URL, 1024*1024)
	info := p.Info()
	if info.ID != "standard_ebooks" || info.AuthRequired {
		t.Fatalf("unexpected Info: %+v", info)
	}
	if !p.ProbeDomain(context.Background(), srv.URL) {
		t.Fatalf("expected ProbeDomain to succeed")
	}

	res, err := p.Search(context.Background(), nil, providers.SearchQuery{Q: "pride", Page: 1, Order: "newest"})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(res.Books) != 1 || res.TotalPages != 2 {
		t.Fatalf("unexpected search result: %+v", res)
	}
	b := res.Books[0]
	if b.Ref != "jane-austen:pride-and-prejudice" || b.Title != "Pride & Prejudice" ||
		len(b.Authors) != 1 || b.Authors[0] != "Jane Austen" ||
		b.CoverURL != srv.URL+"/images/covers/jane-austen_pride.jpg" {
		t.Fatalf("unexpected mapped book: %+v", b)
	}

	// Both colon-slug and raw /ebooks/... path must work in GetBook.
	detail, err := p.GetBook(context.Background(), nil, "jane-austen:pride-and-prejudice")
	if err != nil {
		t.Fatalf("GetBook failed: %v", err)
	}
	if !strings.Contains(detail.Description, "First paragraph") || !strings.Contains(detail.Description, "Second paragraph & more.") {
		t.Fatalf("unexpected description: %q", detail.Description)
	}

	detailRaw, err := p.GetBook(context.Background(), nil, "/ebooks/jane-austen/pride-and-prejudice")
	if err != nil || detailRaw.Ref != "jane-austen:pride-and-prejudice" {
		t.Fatalf("GetBook raw path failed: %+v err=%v", detailRaw, err)
	}

	if _, err := p.GetBook(context.Background(), nil, "missing:book"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	stream, err := p.OpenDownload(context.Background(), nil, "jane-austen:pride-and-prejudice")
	if err != nil {
		t.Fatalf("OpenDownload failed: %v", err)
	}
	data, _ := io.ReadAll(stream.Body)
	_ = stream.Body.Close()

	if !sawSourceDownload {
		t.Fatalf("expected ?source=download query parameter to be appended")
	}
	if lastReferer != srv.URL+"/ebooks/jane-austen/pride-and-prejudice" {
		t.Fatalf("unexpected Referer: %q", lastReferer)
	}
	if stream.Format != "epub" || string(data) != "PK\x03\x04se-epub" {
		t.Fatalf("unexpected download stream: %+v data=%q", stream, string(data))
	}
}
