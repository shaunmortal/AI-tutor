// Command api is the Adigam AI HTTP server.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/config"
	"github.com/shaunmortal/AI-tutor/server/internal/db"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/shed"
	"github.com/shaunmortal/AI-tutor/server/internal/web/content"
	"github.com/shaunmortal/AI-tutor/server/internal/web/learning"
	"github.com/shaunmortal/AI-tutor/server/internal/web/practice"
)

var started = time.Now()

func main() {
	config.LoadDotenv(".env")
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DatabaseURL, cfg.DBTransactionPooler)
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
		Redis: rdb, HTTP: authHTTP(),
	}
	mux := http.NewServeMux()
	deps := &app.Deps{Cfg: cfg, DB: database, Redis: rdb, Auth: verifier, AI: ai.New(cfg.AnthropicAPIKey), Mux: mux}
	learning.Register(deps)
	practice.Register(deps)
	content.Register(deps)
	// Durable ingestion workers (Redis Streams); they stop when ctx is cancelled.
	content.StartWorkers(ctx, deps)
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

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.Fail(w, 404, "NOT_FOUND", "Route not found.")
	})

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           cors(cfg.CORSOrigins, shed.Limit(cfg.MaxInFlight, mux)),
		ReadHeaderTimeout: 10 * time.Second,
		// Generous enough for a 40 MB upload on a slow phone and a 90 s AI answer; finite so a
		// stalled client can't hold a connection for ever.
		ReadTimeout:    2 * time.Minute,
		WriteTimeout:   3 * time.Minute,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
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

// authHTTP is the client for token verification. Go's default keeps only 2 idle connections
// per host, so under load every first-seen token opened a fresh TCP connection to the auth
// server — measured with a million distinct learners: throughput fell 4,300 -> 1,300 req/s and
// port exhaustion turned valid tokens into 401s.
func authHTTP() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 512
	t.MaxIdleConnsPerHost = 512
	return &http.Client{Timeout: 10 * time.Second, Transport: t}
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
