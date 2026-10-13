// Command api is the Adigam AI HTTP server.
package main

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/config"
	"github.com/shaunmortal/AI-tutor/server/internal/db"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

var started = time.Now()

func main() {
	loadDotenv(".env")
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("database", "err", err)
		os.Exit(1)
	}
	defer database.Close()

	ropts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		slog.Error("redis", "err", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(ropts)
	defer rdb.Close()

	verifier := &auth.Verifier{
		SupabaseURL: cfg.SupabaseURL, PublishableKey: cfg.SupabasePublishableKey,
		Redis: rdb, HTTP: &http.Client{Timeout: 10 * time.Second},
	}
	_ = verifier // used by feature routes as they are ported

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		httpx.OK(w, 200, map[string]any{"status": "ok", "uptime": int(time.Since(started).Seconds())})
	})
	// Deep check: is the database reachable too? 503 when not, so a monitor can tell.
	mux.HandleFunc("GET /api/v1/health/deep", func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		if err := database.Admin.Ping(r.Context()); err != nil {
			slog.Error("deep health", "err", err)
			httpx.Fail(w, 503, "DATABASE_UNREACHABLE", "The database did not answer.")
			return
		}
		httpx.OK(w, 200, map[string]any{
			"status": "ok", "database": "reachable",
			"redis":     rdb.Ping(r.Context()).Err() == nil,
			"latencyMs": time.Since(t).Milliseconds(), "uptime": int(time.Since(started).Seconds()),
		})
	})

	// Routes not yet ported to Go fall through to the TypeScript backend.
	// ponytail: strangler proxy; delete with LEGACY_API_URL once every route is ported.
	if cfg.LegacyURL != nil {
		mux.Handle("/api/v1/", httputil.NewSingleHostReverseProxy(cfg.LegacyURL))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.Fail(w, 404, "NOT_FOUND", "Route not found.")
	})

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           cors(cfg.CORSOrigins, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	slog.Info("Adigam API listening", "url", "http://localhost:"+cfg.Port+"/api/v1")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}

func cors(origins []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && slices.Contains(origins, o) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", o)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loadDotenv sets KEY=VALUE lines from path without overriding the real environment.
// Missing file is fine: production sets variables directly.
func loadDotenv(path string) {
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
