// Package app holds what every feature's HTTP handlers share, and how routes are mounted.
package app

import (
	"net/http"

	"github.com/redis/go-redis/v9"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/config"
	"github.com/shaunmortal/AI-tutor/server/internal/db"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

type Deps struct {
	Cfg   *config.Config
	DB    *db.DB
	Redis *redis.Client
	Auth  *auth.Verifier
	AI    *ai.Client // nil when ANTHROPIC_API_KEY is unset; ai methods then return AI_UNAVAILABLE
	Mux   *http.ServeMux
}

// Authed mounts an authenticated route: pattern is e.g. "GET /goals" and is served under
// /api/v1. Extra middleware (rate limits) runs after auth, so it can key on the user.
func (d *Deps) Authed(pattern string, h httpx.Handler, mw ...func(http.Handler) http.Handler) {
	var handler http.Handler = httpx.Wrap(h)
	for i := len(mw) - 1; i >= 0; i-- {
		handler = mw[i](handler)
	}
	d.Mux.Handle(prefix(pattern), d.Auth.Require(handler))
}

func prefix(pattern string) string {
	for i, c := range pattern {
		if c == ' ' {
			return pattern[:i+1] + "/api/v1" + pattern[i+1:]
		}
	}
	return "/api/v1" + pattern
}
