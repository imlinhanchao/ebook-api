package gutendex

import (
	"context"
	"encoding/json"
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

func TestProviderSearchDetailProbeAndDownload(t *testing.T) {
	var lastQuery string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/books":
			if r.URL.RawQuery == "page=1" {
				_, _ = w.Write([]byte(`{"count":1,"results":[]}`))
				return
			}
			lastQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"count": 64,
				"next":  srv.URL + "/books?page=2",
				"results": []map[string]any{
					{
						"id":        1342,
						"title":     "Pride and Prejudice",
						"authors":   []map[string]string{{"name": "Austen, Jane"}},
						"languages": []string{"en"},
						"subjects":  []string{"England -- Fiction", "Love stories"},
						"formats": map[string]string{
							"application/epub+zip": srv.URL + "/files/1342.epub",
							"image/jpeg":           srv.URL + "/covers/1342.jpg",
						},
					},
					{
						"id":        99,
						"title":     "Plain Text Only",
						"authors":   []map[string]string{{"name": "Author B"}},
						"languages": []string{"zh"},
						"formats": map[string]string{
							"text/plain; charset=utf-8": srv.URL + "/files/99.txt",
						},
					},
				},
			})
		case "/books/1342":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        1342,
				"title":     "Pride and Prejudice",
				"authors":   []map[string]string{{"name": "Austen, Jane"}},
				"languages": []string{"en"},
				"subjects":  []string{"Classic"},
				"formats": map[string]string{
					"application/epub+zip": srv.URL + "/files/1342.epub",
				},
			})
		case "/books/99":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":    99,
				"title": "Plain Text Only",
				"formats": map[string]string{
					"text/plain; charset=utf-8": srv.URL + "/files/99.txt",
				},
			})
		case "/books/404":
			http.NotFound(w, r)
		case "/files/1342.epub":
			w.Header().Set("Content-Type", "application/epub+zip")
			_, _ = w.Write([]byte("PK\x03\x04mock-epub-bytes"))
		case "/files/99.txt":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("hello plain text"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := New(srv.URL, 1024*1024)
	info := p.Info()
	if info.ID != "gutendex" || info.AuthRequired || info.DefaultDomain != srv.URL {
		t.Fatalf("unexpected Info(): %+v", info)
	}
	if !p.ProbeDomain(context.Background(), srv.URL) {
		t.Fatalf("expected ProbeDomain to succeed")
	}

	res, err := p.Search(context.Background(), nil, providers.SearchQuery{
		Q:          "Pride",
		Page:       1,
		Extensions: []string{"EPUB"},
		Languages:  []string{"english", "Chinese"},
		Order:      "ascending",
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if !strings.Contains(lastQuery, "mime_type=application%2Fepub") ||
		!strings.Contains(lastQuery, "languages=en%2Czh") ||
		!strings.Contains(lastQuery, "sort=ascending") {
		t.Fatalf("unexpected search query params: %s", lastQuery)
	}
	if res.Page != 1 || res.TotalPages != 2 || len(res.Books) != 2 {
		t.Fatalf("unexpected search result: %+v", res)
	}
	if res.Books[0].Format != "epub" || res.Books[1].Format != "txt" {
		t.Fatalf("unexpected formats: %+v", res.Books)
	}

	detail, err := p.GetBook(context.Background(), nil, "1342")
	if err != nil || detail.Title != "Pride and Prejudice" {
		t.Fatalf("GetBook failed: book=%+v err=%v", detail, err)
	}

	if _, err := p.GetBook(context.Background(), nil, "404"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// EPUB download
	stream, err := p.OpenDownload(context.Background(), nil, "1342")
	if err != nil {
		t.Fatalf("OpenDownload epub failed: %v", err)
	}
	data, _ := io.ReadAll(stream.Body)
	_ = stream.Body.Close()
	if stream.Format != "epub" || string(data) != "PK\x03\x04mock-epub-bytes" || !strings.HasSuffix(stream.Filename, ".epub") {
		t.Fatalf("unexpected epub stream: %+v data=%q", stream, string(data))
	}

	// TXT fallback download
	txtStream, err := p.OpenDownload(context.Background(), nil, "99")
	if err != nil {
		t.Fatalf("OpenDownload txt failed: %v", err)
	}
	txtData, _ := io.ReadAll(txtStream.Body)
	_ = txtStream.Body.Close()
	if txtStream.Format != "txt" || string(txtData) != "hello plain text" {
		t.Fatalf("unexpected txt stream: %+v data=%q", txtStream, string(txtData))
	}

	// Size limit rejection
	capped := New(srv.URL, 4)
	if _, err := capped.OpenDownload(context.Background(), nil, "1342"); err == nil {
		t.Fatalf("expected maxDownloadBytes error")
	}
}
