// Package ratelimit is a per-learner quota for expensive (AI) routes, kept in Redis so
// it survives restarts and holds across instances.
package ratelimit

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// Limit allows max requests per window per learner, keyed on the verified user id —
// never on an IP or a header the caller controls. Must sit behind auth.Require.
func Limit(rdb *redis.Client, name string, max int64, window time.Duration, message string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := fmt.Sprintf("rl:%s:%s", name, auth.UserID(r))
			ctx := r.Context()

			// Fixed window: INCR, and start the clock on the first hit.
			n, err := rdb.Incr(ctx, key).Result()
			if err != nil {
				// Fail open: a Redis outage should slow nobody's studying.
				slog.Warn("rate limit unavailable", "err", err)
				next.ServeHTTP(w, r)
				return
			}
			if n == 1 {
				rdb.Expire(ctx, key, window)
			}
			if n > max {
				ttl, _ := rdb.TTL(ctx, key).Result()
				httpx.Fail(w, http.StatusTooManyRequests, "RATE_LIMITED",
					fmt.Sprintf("%s Try again in %d seconds.", message, int(ttl.Seconds())))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
