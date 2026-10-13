// Command backfill-embeddings embeds the excerpts that have no embedding: those ingested before
// migration 029, or whose embedding failed at ingestion. Safe to rerun; it stops when none is left.
//
// Usage, from server/ (reads server/.env):
//
//	go run ./cmd/backfill-embeddings [-user <learner uuid>]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/config"
	"github.com/shaunmortal/AI-tutor/server/internal/db"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

func main() {
	user := flag.String("user", "", "only this learner's excerpts (default: everyone's)")
	flag.Parse()
	if err := run(*user); err != nil {
		log.Fatal(err)
	}
}

func run(userID string) error {
	config.LoadDotenv(".env")
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	embedder := ai.NewEmbedder(cfg.GeminiAPIKey)
	if embedder == nil {
		return errors.New("GEMINI_API_KEY is not set")
	}
	ctx := context.Background()
	database, err := db.Open(ctx, cfg.DatabaseURL, cfg.DBTransactionPooler)
	if err != nil {
		return err
	}
	defer database.Close()
	sources := store.NewSources(database.Admin)

	done := 0
	for {
		batch, err := sources.ChunksWithoutEmbedding(ctx, userID, 100)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			fmt.Printf("done: %d excerpts embedded\n", done)
			return nil
		}
		docs, ids := make([]ai.Doc, len(batch)), make([]string, len(batch))
		for i, c := range batch {
			docs[i], ids[i] = ai.Doc{Title: c.Title, Text: c.Content}, c.ID
		}
		// The free tier limits tokens per minute; past the client's own retries, wait out the minute.
		var vs [][]float32
		for attempt := 1; ; attempt++ {
			if vs, err = embedder.EmbedDocs(ctx, docs); err == nil || attempt == 5 {
				break
			}
			fmt.Printf("embedding failed (%v), waiting a minute\n", err)
			time.Sleep(time.Minute)
		}
		if err != nil {
			return fmt.Errorf("after %d excerpts: %w", done, err)
		}
		if err := sources.SetEmbeddings(ctx, ids, vs); err != nil {
			return err
		}
		done += len(batch)
		fmt.Printf("%d excerpts embedded\n", done)
	}
}
