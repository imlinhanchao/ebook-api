package api

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/imlinhanchao/ebook-api/internal/providers"
)

func (s *Server) handleDownloadGET(w http.ResponseWriter, r *http.Request) {
	s.executeDownload(w, r, "", nil)
}

func (s *Server) handleDownloadPOST(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CredentialToken string                `json:"credential_token"`
		Credentials     providers.Credentials `json:"credentials"`
	}
	if !decodeJSON(w, r, &body, true) {
		return
	}
	s.executeDownload(w, r, body.CredentialToken, body.Credentials)
}

func (s *Server) executeDownload(w http.ResponseWriter, r *http.Request, bodyToken string, bodyCreds providers.Credentials) {
	id := r.PathValue("id")
	ref := strings.TrimSpace(r.PathValue("ref"))
	p, ok := s.registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "PROVIDER_NOT_FOUND", "未知书源")
		return
	}
	if ref == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REF", "缺少图书引用 ref")
		return
	}
	creds, err := s.resolveCredentials(r, id, bodyToken, bodyCreds)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", err.Error())
		return
	}

	stream, err := p.OpenDownload(r.Context(), creds, ref)
	if err != nil {
		mapProviderError(w, err)
		return
	}
	defer stream.Body.Close()

	contentType := stream.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	if stream.Filename != "" {
		w.Header().Set("Content-Disposition", formatContentDisposition(stream.Filename))
		w.Header().Set("X-Book-Filename", url.PathEscape(stream.Filename))
	}
	if stream.Format != "" {
		w.Header().Set("X-Book-Format", stream.Format)
	}
	if stream.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(stream.Size, 10))
	}
	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, stream.Body); err != nil {
		// Headers and status 200 are already sent; just log the interrupted transfer.
		log.Printf("download stream interrupted for %s/%s: %v", id, ref, err)
	}
}

func formatContentDisposition(filename string) string {
	asciiFallback := toASCIIFilename(filename)
	escapedUTF8 := url.PathEscape(filename)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, asciiFallback, escapedUTF8)
}

func toASCIIFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '"' || r == '\\' || r < 0x20 || r >= 0x7f:
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "book.bin"
	}
	return out
}
