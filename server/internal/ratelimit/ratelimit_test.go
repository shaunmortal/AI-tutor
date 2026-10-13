package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/shaunmortal/AI-tutor/server/internal/auth"
)

func TestLimitPerLearner(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if rdb.Ping(context.Background()).Err() != nil {
		t.Skip("redis not running")
	}
	name := "test-" + time.Now().Format("150405.000000")
	h := Limit(rdb, name, 2, time.Minute, "Slow down.")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	hit := func(user string) int {
		req := httptest.NewRequest("GET", "/", nil)
		req = req.WithContext(auth.WithUserID(req.Context(), user))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	got := []int{hit("a"), hit("a"), hit("a"), hit("b")}
	want := []int{200, 200, 429, 200}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request %d: got %d, want %d (all: %v)", i, got[i], want[i], got)
		}
	}
}
