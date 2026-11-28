package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/imlinhanchao/ebook-api/internal/api"
	"github.com/imlinhanchao/ebook-api/internal/config"
	"github.com/imlinhanchao/ebook-api/internal/providers"
	"github.com/imlinhanchao/ebook-api/internal/providers/gutendex"
	"github.com/imlinhanchao/ebook-api/internal/providers/standardebooks"
	"github.com/imlinhanchao/ebook-api/internal/providers/wenshuoge"
	"github.com/imlinhanchao/ebook-api/internal/providers/zlib"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	reg := registerProviders(cfg)
	if len(reg.List()) == 0 {
		log.Fatal("no book providers enabled; check EBOOK_ENABLED_PROVIDERS")
	}

	srv := api.New(cfg, reg)
	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("ebook-api %s listening on %s (providers: %s, auth: %v)",
			api.Version, cfg.Addr, strings.Join(reg.IDs(), ", "), cfg.APIKey != "")
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down ebook-api...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown error: %v", err)
	}
}

// registerProviders wires all enabled book sources into the registry.
// Add new book sources here after implementing providers.Provider.
func registerProviders(cfg *config.Config) *providers.Registry {
	reg := providers.NewRegistry()

	if cfg.ProviderEnabled("zlib") {
		reg.Register(zlib.New(cfg.ZlibDomains, cfg.MaxDownloadBytes))
	}
	if cfg.ProviderEnabled("gutendex") {
		reg.Register(gutendex.New(cfg.GutendexDomain, cfg.MaxDownloadBytes))
	}
	if cfg.ProviderEnabled("standard_ebooks") {
		reg.Register(standardebooks.New(cfg.StandardEbooksDomain, cfg.MaxDownloadBytes))
	}
	if cfg.ProviderEnabled("wenshuoge") {
		reg.Register(wenshuoge.New(cfg.WenshuogeDomain, cfg.MaxDownloadBytes))
	}

	return reg
}
