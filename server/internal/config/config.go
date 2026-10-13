// Package config reads the environment once at boot and fails fast when it is wrong.
// A missing secret should stop the process, not surface as a confusing 500 later.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	Port        string
	CORSOrigins []string

	SupabaseURL            string
	SupabasePublishableKey string
	SupabaseSecretKey      string // optional: Storage (uploaded course files) needs it
	DatabaseURL            string
	RedisURL               string
	AnthropicAPIKey        string // optional: AI features degrade without it

	// LegacyURL is the TypeScript backend that still serves routes not yet ported.
	// Empty means every route must be served here.
	LegacyURL *url.URL
}

func Load() (*Config, error) {
	c := &Config{
		Port:                   get("PORT", "4000"),
		CORSOrigins:            split(get("CORS_ORIGINS", "http://localhost:5173")),
		SupabaseURL:            strings.TrimRight(os.Getenv("SUPABASE_URL"), "/"),
		SupabasePublishableKey: os.Getenv("SUPABASE_PUBLISHABLE_KEY"),
		SupabaseSecretKey:      os.Getenv("SUPABASE_SECRET_KEY"),
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		RedisURL:               get("REDIS_URL", "redis://localhost:6379/0"),
		AnthropicAPIKey:        os.Getenv("ANTHROPIC_API_KEY"),
	}

	var missing []string
	for name, v := range map[string]string{
		"SUPABASE_URL":             c.SupabaseURL,
		"SUPABASE_PUBLISHABLE_KEY": c.SupabasePublishableKey,
		"DATABASE_URL":             c.DatabaseURL,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	if raw := os.Getenv("LEGACY_API_URL"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("LEGACY_API_URL: %w", err)
		}
		c.LegacyURL = u
	}
	return c, nil
}

// get treats an empty value as absent: `KEY=` in a .env file means "not set".
func get(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
