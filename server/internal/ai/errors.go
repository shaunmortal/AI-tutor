package ai

import (
	"net/http"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// Provider-neutral failures. A learner can't act on a status code, only on whether waiting
// will help, so the messages say that and the details stay in the log.

func timeoutErr() error {
	return httpx.E(http.StatusGatewayTimeout, "AI_TIMEOUT", "The model took too long.")
}

func rateLimited() error {
	return httpx.E(http.StatusTooManyRequests, "AI_RATE_LIMITED", "Too many AI requests right now. Try again shortly.")
}

func unavailable(msg string) error {
	return httpx.E(http.StatusServiceUnavailable, "AI_UNAVAILABLE", msg)
}

func refused() error {
	return httpx.E(http.StatusServiceUnavailable, "AI_REFUSED", "The model declined this request.")
}
