// Package shed keeps the server answering under overload instead of falling over.
//
// Past a point, accepting more work makes every request slower until all of them time out —
// the database pool queues, memory grows, and the process looks dead. Capping requests in
// flight and turning the excess away immediately with 503 + Retry-After keeps latency bounded
// for the requests that are admitted, and the client (lib/api.ts) retries with jittered backoff.
package shed

import (
	"net/http"
	"strings"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// queueWait is how long a request may wait for a slot before being turned away. Long enough to
// absorb a burst, short enough that a refused client finds out at once.
const queueWait = 250 * time.Millisecond

// Limit admits at most max requests at a time. Health checks are never shed, so a load
// balancer can still tell a busy instance from a dead one.
func Limit(max int, next http.Handler) http.Handler {
	slots := make(chan struct{}, max)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/health") || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		timer := time.NewTimer(queueWait)
		defer timer.Stop()
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			next.ServeHTTP(w, r)
		case <-timer.C:
			w.Header().Set("Retry-After", "2")
			httpx.Fail(w, http.StatusServiceUnavailable, "SERVER_BUSY", "Lots of learners right now. Trying again in a moment.")
		case <-r.Context().Done():
		}
	})
}
