package wenshuoge

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

func TestWenshuogeProvider(t *testing.T) {
	var cdnSawCookie bool
	cdnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			cdnSawCookie = true
		}
		w.Header().Set("Content-Type", "application/epub+zip")
		w.Header().Set("Content-Disposition", `attachment; filename="hongloumeng.epub"`)
		_, _ = w.Write([]byte("PK\x03\x04cdn-epub"))
	}))
	defer cdnSrv.Close()

	pollCount := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/v2/auth/csrf":
			http.SetCookie(w, &http.Cookie{Name: "wenshuoge_csrf", Value: "csrf-cookie-1"})
			_, _ = w.Write([]byte(`{"data":{"token":"csrf-tok-1"}}`))
		case "/api/v2/auth/password/login":
			if r.Header.Get("X-CSRF-Token") != "csrf-tok-1" {
				http.Error(w, "missing csrf", http.StatusForbidden)
				return
			}
			c, err := r.Cookie("wenshuoge_csrf")
			if err != nil || c.Value != "csrf-cookie-1" {
				http.Error(w, "missing csrf cookie", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "wenshuoge_session", Value: "sess-abc"})
			_, _ = w.Write([]byte(`{"data":{"user":{"email":"reader@example.org"}}}`))
		case "/api/v2/downloads/quota":
			c, err := r.Cookie("wenshuoge_session")
			if err != nil || c.Value != "sess-abc" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"login_required","message":"请先登录"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"used":1,"limit":5,"remaining":4}}`))
		case "/api/v2/auth/status":
			_, _ = w.Write([]byte(`{"data":{"user":{"email":"reader@example.org"}}}`))
		case "/api/v2/catalog":
			cursor := r.URL.Query().Get("cursor")
			if cursor == "cur-2" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"books": []map[string]any{
							{"id": "ed2", "title": "三国演义", "author": "罗贯中", "era": "明", "category": "古典小说"},
						},
					},
					"meta": map[string]any{"total": 3, "nextCursor": ""},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"books": []map[string]any{
						{
							"id":           "ed1",
							"title":        "红楼梦",
							"author":       "曹雪芹，高鹗",
							"era":          "清",
							"category":     "古典名著",
							"variantLabel": "简体整理本",
							"tags":         []string{"章回小说", "四大名著"},
						},
					},
				},
				"meta": map[string]any{"total": 3, "nextCursor": "cur-2"},
			})
		case "/book/ed1":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><head><script type="application/ld+json">{"name":"红楼梦","author":{"name":"曹雪芹"},"description":"满纸荒唐言","image":"/covers/ed1.jpg"}</script></head><body><h1>红楼梦</h1></body></html>`))
		case "/api/v2/downloads/epub/authorize":
			var body struct {
				EditionID string `json:"editionId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.EditionID == "quota-err" {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":"download_daily_limit","message":"今日额度已满"}}`))
				return
			}
			pollCount = 0
			_, _ = w.Write([]byte(`{"data":{"state":"queued","intent":"job-99"}}`))
		case "/api/v2/downloads/epub/status":
			pollCount++
			if pollCount == 1 {
				_, _ = w.Write([]byte(`{"data":{"state":"queued"}}`))
				return
			}
			// Redirect via same-origin download hop to cross-origin CDN to verify Cookie stripping.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"state": "ready",
					"url":   srv.URL + "/redirect-to-cdn",
				},
			})
		case "/redirect-to-cdn":
			http.Redirect(w, r, cdnSrv.URL+"/ed1.epub", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := New(srv.URL, 1024*1024)
	p.pollInterval = 2 * time.Millisecond

	info := p.Info()
	if info.ID != "wenshuoge" || info.AuthRequired || len(info.AuthModes) != 2 {
		t.Fatalf("unexpected Info: %+v", info)
	}
	if !p.ProbeDomain(context.Background(), srv.URL) {
		t.Fatalf("expected ProbeDomain to pass")
	}

	// Search works anonymously + caches cursor for page 2
	p1, err := p.Search(context.Background(), nil, providers.SearchQuery{Q: "红楼梦", Page: 1, Limit: 1})
	if err != nil || len(p1.Books) != 1 || p1.TotalPages != 3 {
		t.Fatalf("page 1 search failed: %+v err=%v", p1, err)
	}
	if len(p1.Books[0].Authors) != 2 || p1.Books[0].Format != "epub" {
		t.Fatalf("unexpected authors/format: %+v", p1.Books[0])
	}

	p2, err := p.Search(context.Background(), nil, providers.SearchQuery{Q: "红楼梦", Page: 2, Limit: 1})
	if err != nil || len(p2.Books) != 1 || p2.Books[0].Title != "三国演义" {
		t.Fatalf("page 2 cursor search failed: %+v err=%v", p2, err)
	}

	// Detail JSON-LD extraction
	detail, err := p.GetBook(context.Background(), nil, "ed1")
	if err != nil || detail.Description != "满纸荒唐言" || detail.CoverURL != srv.URL+"/covers/ed1.jpg" {
		t.Fatalf("GetBook failed: %+v err=%v", detail, err)
	}

	// Missing creds returns ErrNotConfigured
	if _, err := p.OpenDownload(context.Background(), nil, "ed1"); !errors.Is(err, providers.ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}

	// Login & Verify
	creds, err := p.Login(context.Background(), srv.URL, "reader@example.org", "secret")
	if err != nil || creds["wenshuoge_session"] != "sess-abc" {
		t.Fatalf("Login failed: creds=%+v err=%v", creds, err)
	}
	acc, err := p.Verify(context.Background(), creds)
	if err != nil || acc.DailyRemaining != 4 || acc.Email != "reader@example.org" {
		t.Fatalf("Verify failed: %+v err=%v", acc, err)
	}
	if _, err := p.Verify(context.Background(), providers.Credentials{"wenshuoge_session": "expired"}); !errors.Is(err, providers.ErrSessionExpired) {
		t.Fatalf("expected ErrSessionExpired, got %v", err)
	}

	// Quota exceeded mapping
	if _, err := p.OpenDownload(context.Background(), creds, "quota-err"); !errors.Is(err, providers.ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}

	// Async poll download + cross-origin cookie stripping check
	stream, err := p.OpenDownload(context.Background(), creds, "ed1")
	if err != nil {
		t.Fatalf("OpenDownload failed: %v", err)
	}
	data, _ := io.ReadAll(stream.Body)
	_ = stream.Body.Close()
	if string(data) != "PK\x03\x04cdn-epub" || stream.Filename != "hongloumeng.epub" || !strings.HasSuffix(stream.Filename, ".epub") {
		t.Fatalf("unexpected download result: %+v data=%q", stream, string(data))
	}
	if cdnSawCookie {
		t.Fatalf("expected Cookie header to be stripped on cross-origin CDN redirect")
	}
}
