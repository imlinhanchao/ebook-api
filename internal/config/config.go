package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"

	"github.com/imlinhanchao/ebook-api/internal/providers"
	"github.com/imlinhanchao/ebook-api/internal/secret"
)

// Config holds the resolved runtime configuration for ebook-api.
type Config struct {
	Addr                  string
	DataDir               string
	StaticDir             string
	APIKey                string
	Secret                []byte
	AllowPlainCredHeaders bool
	PublicDownload        bool
	DownloadCacheControl  string
	DefaultProvider       string
	EnabledProviders      []string
	MaxDownloadBytes      int64
	CORSOrigins           []string
	ZlibDomains           []string
	DefaultCreds          map[string]providers.Credentials
}

// Load reads .env (if present) and environment variables, then loads or
// creates the 32-byte AES-256-GCM key used for credential tokens.
func Load() (*Config, error) {
	_ = loadDotEnv(".env")
	cfg := LoadFromEnv()

	key, err := secret.LoadOrCreateKey(os.Getenv("EBOOK_SECRET"), cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.Secret = key
	return cfg, nil
}

// LoadFromEnv builds a Config solely from the current process environment
// without touching the filesystem.
func LoadFromEnv() *Config {
	maxMB := envInt("EBOOK_MAX_DOWNLOAD_MB", 500)
	var maxBytes int64
	if maxMB > 0 {
		maxBytes = int64(maxMB) * 1024 * 1024
	}

	cfg := &Config{
		Addr:                  env("EBOOK_ADDR", "127.0.0.1:8091"),
		DataDir:               env("EBOOK_DATA_DIR", "data"),
		StaticDir:             env("EBOOK_STATIC_DIR", "static"),
		APIKey:                strings.TrimSpace(os.Getenv("EBOOK_API_KEY")),
		AllowPlainCredHeaders: envBool("EBOOK_ALLOW_PLAIN_CRED_HEADERS", false),
		PublicDownload:        envBool("EBOOK_PUBLIC_DOWNLOAD", false),
		DownloadCacheControl:  env("EBOOK_DOWNLOAD_CACHE_CONTROL", "public, max-age=2592000, immutable"),
		DefaultProvider:       strings.ToLower(strings.TrimSpace(env("EBOOK_DEFAULT_PROVIDER", "zlib"))),
		EnabledProviders:      splitCSVLower(os.Getenv("EBOOK_ENABLED_PROVIDERS")),
		MaxDownloadBytes:      maxBytes,
		CORSOrigins:           splitCSV(os.Getenv("EBOOK_CORS_ORIGINS")),
		DefaultCreds:          make(map[string]providers.Credentials),
	}

	rawDomains := env("ZLIB_DOMAINS",
		"https://z-lib.gd,https://z-lib.gl,https://z-library.ec,https://zlib.bz")
	for _, d := range splitCSV(rawDomains) {
		cfg.ZlibDomains = append(cfg.ZlibDomains, strings.TrimRight(d, "/"))
	}

	// Generic provider default credentials: EBOOK_CRED_<PROVIDER>_<KEY>=<value>
	for _, pair := range os.Environ() {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || !strings.HasPrefix(k, "EBOOK_CRED_") || strings.TrimSpace(v) == "" {
			continue
		}
		rest := strings.TrimPrefix(k, "EBOOK_CRED_")
		providerID, credKey, ok := strings.Cut(rest, "_")
		if !ok || providerID == "" || credKey == "" {
			continue
		}
		cfg.setDefaultCred(strings.ToLower(providerID), strings.ToLower(credKey), strings.TrimSpace(v))
	}

	// Convenient Z-Library aliases (do not overwrite explicit EBOOK_CRED_ZLIB_*).
	cfg.setDefaultCredIfMissing("zlib", "domain", strings.TrimRight(strings.TrimSpace(os.Getenv("ZLIB_DOMAIN")), "/"))
	cfg.setDefaultCredIfMissing("zlib", "remix_userid", strings.TrimSpace(os.Getenv("ZLIB_REMIX_USERID")))
	cfg.setDefaultCredIfMissing("zlib", "remix_userkey", strings.TrimSpace(os.Getenv("ZLIB_REMIX_USERKEY")))

	return cfg
}

// ProviderEnabled reports whether providerID should be registered.
func (c *Config) ProviderEnabled(providerID string) bool {
	if len(c.EnabledProviders) == 0 {
		return true
	}
	target := strings.ToLower(strings.TrimSpace(providerID))
	for _, id := range c.EnabledProviders {
		if id == target {
			return true
		}
	}
	return false
}

// ProviderDefaultCreds returns a copy of the configured default credentials
// for providerID (may be empty).
func (c *Config) ProviderDefaultCreds(providerID string) providers.Credentials {
	if c == nil {
		return nil
	}
	creds, ok := c.DefaultCreds[strings.ToLower(strings.TrimSpace(providerID))]
	if !ok || len(creds) == 0 {
		return nil
	}
	return creds.Clone()
}

func (c *Config) setDefaultCred(providerID, key, value string) {
	if value == "" {
		return
	}
	if c.DefaultCreds[providerID] == nil {
		c.DefaultCreds[providerID] = make(providers.Credentials)
	}
	c.DefaultCreds[providerID][key] = value
}

func (c *Config) setDefaultCredIfMissing(providerID, key, value string) {
	if value == "" {
		return
	}
	if existing, ok := c.DefaultCreds[providerID]; ok && existing[key] != "" {
		return
	}
	c.setDefaultCred(providerID, key, value)
}

func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
	return sc.Err()
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
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

func splitCSVLower(s string) []string {
	parts := splitCSV(s)
	for i, p := range parts {
		parts[i] = strings.ToLower(p)
	}
	return parts
}
