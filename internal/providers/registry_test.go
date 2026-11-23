package providers

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type stubProvider struct {
	id   string
	name string
}

func (s stubProvider) ID() string             { return s.id }
func (s stubProvider) DisplayName() string    { return s.name }
func (s stubProvider) Capabilities() []string { return []string{"search", "download"} }
func (s stubProvider) Search(context.Context, Credentials, SearchQuery) (SearchResult, error) {
	return SearchResult{}, nil
}
func (s stubProvider) GetBook(context.Context, Credentials, string) (Book, error) {
	return Book{}, nil
}
func (s stubProvider) OpenDownload(context.Context, Credentials, string) (*DownloadStream, error) {
	return &DownloadStream{Body: io.NopCloser(strings.NewReader("ok")), Size: 2}, nil
}

func TestRegistryOrderAndLookup(t *testing.T) {
	reg := NewRegistry()
	reg.Register(stubProvider{id: "zlib", name: "Z-Library"})
	reg.Register(stubProvider{id: "gutenberg", name: "Project Gutenberg"})

	ids := reg.IDs()
	if len(ids) != 2 || ids[0] != "gutenberg" || ids[1] != "zlib" {
		t.Fatalf("expected sorted IDs [gutenberg zlib], got %v", ids)
	}
	p, ok := reg.Get("zlib")
	if !ok || p.DisplayName() != "Z-Library" {
		t.Fatalf("lookup zlib failed: ok=%v p=%v", ok, p)
	}
	if _, ok := reg.Get("missing"); ok {
		t.Fatalf("expected missing provider lookup to return false")
	}
	info := Describe(p)
	if info.ID != "zlib" || info.Name != "Z-Library" || len(info.Capabilities) != 2 {
		t.Fatalf("unexpected Describe output: %+v", info)
	}
	if !errors.Is(ErrUnknownProvider("missing"), ErrUnsupported) {
		t.Fatalf("ErrUnknownProvider should wrap ErrUnsupported")
	}
}
