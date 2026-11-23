package config

import (
	"testing"
)

func TestLoadFromEnvDefaultsAndCreds(t *testing.T) {
	t.Setenv("EBOOK_ADDR", "0.0.0.0:9090")
	t.Setenv("EBOOK_API_KEY", "top-secret")
	t.Setenv("EBOOK_DEFAULT_PROVIDER", "zlib")
	t.Setenv("EBOOK_ENABLED_PROVIDERS", "zlib, gutenberg")
	t.Setenv("EBOOK_MAX_DOWNLOAD_MB", "100")
	t.Setenv("ZLIB_DOMAINS", "https://a.example/, https://b.example")
	t.Setenv("ZLIB_REMIX_USERID", "123")
	t.Setenv("ZLIB_REMIX_USERKEY", "abc")
	t.Setenv("EBOOK_CRED_CUSTOM_API_KEY", "custom-key")

	cfg := LoadFromEnv()
	if cfg.Addr != "0.0.0.0:9090" {
		t.Fatalf("Addr = %q", cfg.Addr)
	}
	if cfg.APIKey != "top-secret" {
		t.Fatalf("APIKey = %q", cfg.APIKey)
	}
	if cfg.MaxDownloadBytes != 100*1024*1024 {
		t.Fatalf("MaxDownloadBytes = %d", cfg.MaxDownloadBytes)
	}
	if !cfg.ProviderEnabled("zlib") || !cfg.ProviderEnabled("gutenberg") || cfg.ProviderEnabled("other") {
		t.Fatalf("ProviderEnabled mismatch: %v", cfg.EnabledProviders)
	}
	if len(cfg.ZlibDomains) != 2 || cfg.ZlibDomains[0] != "https://a.example" {
		t.Fatalf("ZlibDomains = %v", cfg.ZlibDomains)
	}
	zCreds := cfg.ProviderDefaultCreds("zlib")
	if zCreds["remix_userid"] != "123" || zCreds["remix_userkey"] != "abc" {
		t.Fatalf("zlib creds = %v", zCreds)
	}
	cCreds := cfg.ProviderDefaultCreds("custom")
	if cCreds["api_key"] != "custom-key" {
		t.Fatalf("custom creds = %v", cCreds)
	}
}
