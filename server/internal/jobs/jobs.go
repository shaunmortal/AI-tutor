// Package jobs is a small durable work queue on Redis Streams.
//
// Why not a goroutine: ingesting a lecture video or a 300-page PDF takes minutes, and a
// goroutine dies with the process — a deploy or crash used to leave a source stuck on
// "processing" for ever. A stream entry is only acknowledged once its handler succeeds,
// so a job whose worker died is still pending and gets reclaimed by the next worker.
//
// Semantics: at-least-once. Handlers must be idempotent (re-running an ingestion for the
// same source must replace, not duplicate, its chunks).
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

const group = "workers"

type Queue struct {
	rdb    *redis.Client
	stream string

	// A job not acknowledged within Visibility is assumed dead and handed to another worker.
	Visibility time.Duration
	// After MaxAttempts deliveries the job is given up on and OnGiveUp is called.
	MaxAttempts int64
	// OnGiveUp records the permanent failure (e.g. marks the source failed). May be nil.
	OnGiveUp func(ctx context.Context, payload []byte, err error)
}

func New(rdb *redis.Client, stream string) *Queue {
	return &Queue{rdb: rdb, stream: stream, Visibility: 10 * time.Minute, MaxAttempts: 3}
}

// Enqueue adds a job; v is JSON-encoded.
func (q *Queue) Enqueue(ctx context.Context, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return q.rdb.XAdd(ctx, &redis.XAddArgs{Stream: q.stream, Values: map[string]any{"p": payload}}).Err()
}

// Run processes jobs until ctx is cancelled. Safe to run on several instances at once:
// the consumer group gives each job to one worker at a time.
func (q *Queue) Run(ctx context.Context, handle func(ctx context.Context, payload []byte) error) {
	if err := q.rdb.XGroupCreateMkStream(ctx, q.stream, group, "0").Err(); err != nil &&
		err.Error() != "BUSYGROUP Consumer Group name already exists" {
		slog.Error("jobs: create group", "stream", q.stream, "err", err)
	}
	host, _ := os.Hostname()
	consumer := fmt.Sprintf("%s-%d", host, os.Getpid())

	for ctx.Err() == nil {
		// Reclaim first: jobs whose worker died mid-way.
		msgs, _, err := q.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream: q.stream, Group: group, Consumer: consumer, MinIdle: q.Visibility, Start: "0", Count: 1,
		}).Result()
		if err != nil && !errors.Is(err, redis.Nil) && ctx.Err() == nil {
			slog.Warn("jobs: autoclaim", "err", err)
		}
		if len(msgs) == 0 {
			// Block no longer than Visibility, so a failed job is retried on time.
			streams, err := q.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
				Group: group, Consumer: consumer, Streams: []string{q.stream, ">"}, Count: 1, Block: min(q.Visibility, 5*time.Second),
			}).Result()
			if err != nil {
				if !errors.Is(err, redis.Nil) && ctx.Err() == nil {
					slog.Warn("jobs: read", "err", err)
					time.Sleep(time.Second) // Redis down: don't spin
				}
				continue
			}
			msgs = streams[0].Messages
		}
		for _, m := range msgs {
			q.process(ctx, m, handle)
		}
	}
}

func (q *Queue) process(ctx context.Context, m redis.XMessage, handle func(context.Context, []byte) error) {
	payload := []byte(fmt.Sprint(m.Values["p"]))
	err := handle(ctx, payload)
	if err == nil {
		q.ack(m.ID)
		return
	}
	attempts := int64(1)
	if p, perr := q.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: q.stream, Group: group, Start: m.ID, End: m.ID, Count: 1,
	}).Result(); perr == nil && len(p) == 1 {
		attempts = p[0].RetryCount
	}
	slog.Warn("jobs: failed", "stream", q.stream, "id", m.ID, "attempt", attempts, "err", err)
	if attempts >= q.MaxAttempts {
		if q.OnGiveUp != nil {
			q.OnGiveUp(ctx, payload, err)
		}
		q.ack(m.ID)
	}
	// Otherwise leave it pending; XAutoClaim retries it after Visibility.
}

func (q *Queue) ack(id string) {
	// Background context: a job that finished must be acknowledged even during shutdown.
	ctx := context.Background()
	q.rdb.XAck(ctx, q.stream, group, id)
	q.rdb.XDel(ctx, q.stream, id)
}
