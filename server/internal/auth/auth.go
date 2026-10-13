// Package auth turns a bearer token into a verified user id.
//
// The rule this package exists to enforce: the learner's id comes from the token and
// nowhere else. A user_id in a body, query string or custom header is ignored.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

type ctxKey struct{}

// Long enough to be a real JWT, short enough to reject junk before calling out.
const minToken, maxToken = 20, 4096

// cacheFor bounds how long a revoked token keeps working. One verification call per
// token per minute instead of one per request.
const cacheFor = time.Minute

type Verifier struct {
	SupabaseURL    string
	PublishableKey string
	Redis          *redis.Client
	HTTP           *http.Client
}

// Require rejects the request unless it carries a valid Supabase access token.
func (v *Verifier) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		token = strings.TrimSpace(token)
		if !ok || len(token) < minToken || len(token) > maxToken {
			httpx.Fail(w, http.StatusUnauthorized, "UNAUTHENTICATED", "Missing or malformed bearer token.")
			return
		}
		userID, err := v.verify(r.Context(), token)
		if err != nil || userID == "" {
			httpx.Fail(w, http.StatusUnauthorized, "UNAUTHENTICATED", "Invalid or expired token.")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUserID(r.Context(), userID)))
	})
}

// WithUserID attaches a verified id. Only Require and tests should call it.
func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// UserID reads the identity Require established. It panics when the middleware is
// missing, so a handler can never quietly run unauthenticated.
func UserID(r *http.Request) string {
	id, ok := r.Context().Value(ctxKey{}).(string)
	if !ok {
		panic("auth: route is missing the Require middleware")
	}
	return id
}

func (v *Verifier) verify(ctx context.Context, token string) (string, error) {
	sum := sha256.Sum256([]byte(token))
	key := "auth:" + hex.EncodeToString(sum[:])

	if id, err := v.Redis.Get(ctx, key).Result(); err == nil {
		return id, nil
	}

	// ponytail: asks Supabase to validate the token (one HTTP call per token per minute).
	// Verifying the signature against the project JWKS would remove the hop entirely.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, v.SupabaseURL+"/auth/v1/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("apikey", v.PublishableKey)
	res, err := v.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", nil
	}
	var user struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(res.Body).Decode(&user); err != nil {
		return "", err
	}
	// A Redis outage must not lock everybody out, so a failed cache write is ignored.
	_ = v.Redis.Set(ctx, key, user.ID, cacheFor).Err()
	return user.ID, nil
}
