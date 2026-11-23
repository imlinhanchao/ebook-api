package api

import (
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/imlinhanchao/ebook-api/internal/providers"
)

type searchRequest struct {
	Q                          string                           `json:"q"`
	Provider                   string                           `json:"provider"`
	Providers                  []string                         `json:"providers"`
	Page                       int                              `json:"page"`
	Limit                      int                              `json:"limit"`
	Ext                        []string                         `json:"ext"`
	Lang                       []string                         `json:"lang"`
	YearFrom                   int                              `json:"year_from"`
	YearTo                     int                              `json:"year_to"`
	Order                      string                           `json:"order"`
	CredentialToken            string                           `json:"credential_token"`
	CredentialTokensByProvider map[string]string                `json:"credential_tokens_by_provider"`
	Credentials                providers.Credentials            `json:"credentials"`
	CredentialsByProvider      map[string]providers.Credentials `json:"credentials_by_provider"`
}

type providerSearchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type providerSearchBucket struct {
	Provider   string               `json:"provider"`
	Books      []providers.Book     `json:"books"`
	Page       int                  `json:"page"`
	TotalPages int                  `json:"total_pages"`
	Error      *providerSearchError `json:"error,omitempty"`
}

func (s *Server) handleSearchGET(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := searchRequest{
		Q:         strings.TrimSpace(q.Get("q")),
		Provider:  strings.TrimSpace(q.Get("provider")),
		Providers: splitCSV(q.Get("providers")),
		Page:      atoi(q.Get("page"), 1),
		Limit:     atoi(q.Get("limit"), 20),
		Ext:       splitCSV(q.Get("ext")),
		Lang:      splitCSV(q.Get("lang")),
		YearFrom:  atoi(q.Get("year_from"), 0),
		YearTo:    atoi(q.Get("year_to"), 0),
		Order:     strings.TrimSpace(q.Get("order")),
	}
	s.executeSearch(w, r, req)
}

func (s *Server) handleSearchPOST(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	req.Q = strings.TrimSpace(req.Q)
	req.Provider = strings.TrimSpace(req.Provider)
	req.Order = strings.TrimSpace(req.Order)
	s.executeSearch(w, r, req)
}

func (s *Server) executeSearch(w http.ResponseWriter, r *http.Request, req searchRequest) {
	if req.Q == "" {
		writeError(w, http.StatusBadRequest, "INVALID_QUERY", "请输入搜索关键词 q")
		return
	}

	sq := providers.SearchQuery{
		Q:          req.Q,
		Page:       clampInt(defaultInt(req.Page, 1), 1, 1000),
		Limit:      clampInt(defaultInt(req.Limit, 20), 1, 100),
		Extensions: req.Ext,
		Languages:  req.Lang,
		YearFrom:   req.YearFrom,
		YearTo:     req.YearTo,
		Order:      req.Order,
	}

	targets, multi, errCode, errMsg := s.resolveSearchProviders(req.Provider, req.Providers)
	if errCode != "" {
		writeError(w, http.StatusNotFound, errCode, errMsg)
		return
	}

	// Single-provider search returns a flat, backward-friendly response.
	if !multi {
		p := targets[0]
		bodyToken, bodyCreds := scopedBodyAuth(req, p.ID())
		creds, err := s.resolveCredentials(r, p.ID(), bodyToken, bodyCreds)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_CREDENTIALS", err.Error())
			return
		}
		res, err := p.Search(r.Context(), creds, sq)
		if err != nil {
			mapProviderError(w, err)
			return
		}
		books := res.Books
		if books == nil {
			books = []providers.Book{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"provider":    p.ID(),
			"books":       books,
			"page":        res.Page,
			"total_pages": res.TotalPages,
		})
		return
	}

	// Multi-provider fan-out search: query all target providers concurrently.
	results := make([]providerSearchBucket, len(targets))
	var wg sync.WaitGroup
	for i, p := range targets {
		wg.Add(1)
		go func(idx int, prov providers.Provider) {
			defer wg.Done()
			bodyToken, bodyCreds := scopedBodyAuth(req, prov.ID())
			creds, err := s.resolveCredentials(r, prov.ID(), bodyToken, bodyCreds)
			if err != nil {
				results[idx] = providerSearchBucket{
					Provider: prov.ID(),
					Books:    []providers.Book{},
					Error:    &providerSearchError{Code: "INVALID_CREDENTIALS", Message: err.Error()},
				}
				return
			}
			res, err := prov.Search(r.Context(), creds, sq)
			if err != nil {
				_, code, msg := classifyProviderError(err)
				results[idx] = providerSearchBucket{
					Provider: prov.ID(),
					Books:    []providers.Book{},
					Error:    &providerSearchError{Code: code, Message: msg},
				}
				return
			}
			books := res.Books
			if books == nil {
				books = []providers.Book{}
			}
			results[idx] = providerSearchBucket{
				Provider:   prov.ID(),
				Books:      books,
				Page:       res.Page,
				TotalPages: res.TotalPages,
			}
		}(i, p)
	}
	wg.Wait()

	merged := make([]providers.Book, 0)
	for _, bucket := range results {
		merged = append(merged, bucket.Books...)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"books":   merged,
		"results": results,
	})
}

func scopedBodyAuth(req searchRequest, providerID string) (string, providers.Credentials) {
	token := req.CredentialToken
	if scoped, ok := req.CredentialTokensByProvider[providerID]; ok && strings.TrimSpace(scoped) != "" {
		token = scoped
	}
	creds := req.Credentials
	if scoped, ok := req.CredentialsByProvider[providerID]; ok && len(scoped) > 0 {
		creds = scoped
	}
	return token, creds
}

func (s *Server) resolveSearchProviders(single string, many []string) ([]providers.Provider, bool, string, string) {
	if len(many) > 0 {
		seen := make(map[string]struct{}, len(many))
		out := make([]providers.Provider, 0, len(many))
		for _, id := range many {
			id = strings.ToLower(strings.TrimSpace(id))
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			p, ok := s.registry.Get(id)
			if !ok {
				return nil, false, "PROVIDER_NOT_FOUND", "未知书源: " + id
			}
			seen[id] = struct{}{}
			out = append(out, p)
		}
		if len(out) > 0 {
			return out, len(out) > 1, "", ""
		}
	}

	if strings.EqualFold(single, "all") || single == "*" {
		all := s.registry.List()
		if len(all) == 0 {
			return nil, false, "PROVIDER_NOT_FOUND", "当前没有已启用的书源"
		}
		return all, true, "", ""
	}

	id := strings.ToLower(single)
	if id == "" {
		id = s.cfg.DefaultProvider
	}
	if p, ok := s.registry.Get(id); ok {
		return []providers.Provider{p}, false, "", ""
	}
	if single == "" {
		if all := s.registry.List(); len(all) > 0 {
			return []providers.Provider{all[0]}, false, "", ""
		}
	}
	return nil, false, "PROVIDER_NOT_FOUND", "未知书源: " + id
}

func (s *Server) handleBookDetailGET(w http.ResponseWriter, r *http.Request) {
	s.executeBookDetail(w, r, "", nil)
}

func (s *Server) handleBookDetailPOST(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CredentialToken string                `json:"credential_token"`
		Credentials     providers.Credentials `json:"credentials"`
	}
	if !decodeJSON(w, r, &body, true) {
		return
	}
	s.executeBookDetail(w, r, body.CredentialToken, body.Credentials)
}

func (s *Server) executeBookDetail(w http.ResponseWriter, r *http.Request, bodyToken string, bodyCreds providers.Credentials) {
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
	book, err := p.GetBook(r.Context(), creds, ref)
	if err != nil {
		mapProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"book": book})
}

func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func defaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
