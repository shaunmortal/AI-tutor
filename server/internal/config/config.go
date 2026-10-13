// Package config reads the environment once at boot and fails fast when it is wrong.
// A missing secret should stop the process, not surface as a confusing 500 later.
package config

import (
	"bufio"
	"fmt"
	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"os"
	"strconv"
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
	GeminiAPIKey           string
	AIProvider             string // claude (default) or gemini
	AIModelHigh            string // optional model overrides
	AIModelStandard        string

	// MaxInFlight caps concurrent requests per instance; the excess gets 503 + Retry-After.
	MaxInFlight int
	// DBTransactionPooler: DATABASE_URL points at a transaction-mode pooler (Supabase :6543,
	// PgBouncer). Needed past a few hundred connections; disables prepared statements.
	DBTransactionPooler bool
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
		GeminiAPIKey:           os.Getenv("GEMINI_API_KEY"),
		AIProvider:             get("AI_PROVIDER", "claude"),
		AIModelHigh:            os.Getenv("AI_MODEL_HIGH"),
		AIModelStandard:        os.Getenv("AI_MODEL_STANDARD"),
		MaxInFlight:            256,
		DBTransactionPooler:    os.Getenv("DB_POOL_MODE") == "transaction",
	}
	if c.AIProvider != "claude" && c.AIProvider != "gemini" {
		return nil, fmt.Errorf("AI_PROVIDER: want claude or gemini, got %q", c.AIProvider)
	}
	if v := os.Getenv("MAX_IN_FLIGHT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("MAX_IN_FLIGHT: want a positive integer, got %q", v)
		}
		c.MaxInFlight = n
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

	return c, nil
}

// get treats an empty value as absent: `KEY=` in a .env file means "not set".
// AISettings is what ai.New needs, read from the environment.
func (c *Config) AISettings() ai.Settings {
	return ai.Settings{Provider: c.AIProvider, AnthropicKey: c.AnthropicAPIKey, GeminiKey: c.GeminiAPIKey,
		HighModel: c.AIModelHigh, StandardModel: c.AIModelStandard}
}

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

// LoadDotenv sets KEY=VALUE lines from path without overriding the real environment.
// Missing file is fine: production sets variables directly.
func LoadDotenv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
}
