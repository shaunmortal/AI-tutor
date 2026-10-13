package jobs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testQueue(t *testing.T) *Queue {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if rdb.Ping(context.Background()).Err() != nil {
		t.Skip("redis not running")
	}
	stream := "test-jobs-" + t.Name() + time.Now().Format("150405.000000")
	t.Cleanup(func() { rdb.Del(context.Background(), stream) })
	q := New(rdb, stream)
	q.Visibility = 50 * time.Millisecond
	return q
}

func TestRetriesThenSucceeds(t *testing.T) {
	q := testQueue(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var calls atomic.Int32
	done := make(chan string, 1)
	go q.Run(ctx, func(_ context.Context, p []byte) error {
		if calls.Add(1) < 2 {
			return errors.New("transient") // first delivery fails, as if the worker crashed
		}
		done <- string(p)
		return nil
	})
	if err := q.Enqueue(ctx, map[string]string{"sourceId": "s1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-done:
		if p != `{"sourceId":"s1"}` {
			t.Fatalf("payload = %s", p)
		}
	case <-ctx.Done():
		t.Fatalf("job never succeeded (calls=%d)", calls.Load())
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	q := testQueue(t)
	q.MaxAttempts = 2
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gaveUp := make(chan error, 1)
	q.OnGiveUp = func(_ context.Context, _ []byte, err error) { gaveUp <- err }
	go q.Run(ctx, func(context.Context, []byte) error { return errors.New("broken file") })
	_ = q.Enqueue(ctx, "x")

	select {
	case err := <-gaveUp:
		if err.Error() != "broken file" {
			t.Fatalf("err = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("never gave up")
	}
	time.Sleep(100 * time.Millisecond)
	if n, _ := q.rdb.XLen(context.Background(), q.stream).Result(); n != 0 {
		t.Fatalf("given-up job still in stream (%d)", n)
	}
}
