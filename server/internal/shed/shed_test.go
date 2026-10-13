package shed

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestShedsBeyondCapacity(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	h := Limit(2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
	}))

	var wg sync.WaitGroup
	for range 2 { // fill both slots
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/goals", nil))
		}()
	}
	<-started
	<-started

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/goals", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("third request: got %d, Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
	}

	go func() { release <- struct{}{}; release <- struct{}{} }()
	wg.Wait()
}

func TestHealthNeverShed(t *testing.T) {
	block := make(chan struct{})
	h := Limit(1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			<-block
		}
	}))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/goals", nil))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/health", nil))
	close(block)
	if rec.Code != http.StatusOK {
		t.Fatalf("health got %d", rec.Code)
	}
}
