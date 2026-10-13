package content

// Revision flashcards: written once per topic per learner from that topic's own excerpts, each
// citing the excerpt it came from, and reviewed into the same SM-2 schedule practice moves.

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/dates"
	"github.com/shaunmortal/AI-tutor/server/internal/goals"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/learner"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

const (
	// Topics in one deck: enough for a sitting, few enough to write in parallel on first open.
	deckTopics = 4
	// Below this mastery a topic is weak enough to revise from cards before it is due.
	weakMastery     = 60
	cardsPerTopic   = 6
	excerptsPerDeck = 8
	excerptChars    = 1500
)

type cardView struct {
	ID            string        `json:"id"`
	Front         string        `json:"front"`
	Back          string        `json:"back"`
	SourceChunkID *string       `json:"sourceChunkId"`
	Citations     []ai.Citation `json:"citations"`
}

type deckTopic struct {
	store.RevisionTopic
	Cards []cardView `json:"cards"`
}

// learnerDay is the learner's today and days to their exam (nil without a goal).
func (s *svc) learnerDay(ctx context.Context, userID string) (string, *int, error) {
	var daysUntilExam *int
	err := s.d.DB.AsUser(ctx, userID, func(tx pgx.Tx) error {
		g, err := goals.New(store.NewGoals(tx)).GetActive(ctx, userID)
		if g != nil {
			daysUntilExam = &g.DaysRemaining
		}
		return err
	})
	if err != nil {
		return "", nil, err
	}
	tz := ""
	if t, err := store.NewLearners(s.d.DB.Admin).FindTimezone(ctx, userID); err == nil && t != nil {
		tz = *t
	}
	return dates.TodayIn(tz, time.Now()), daysUntilExam, nil
}

// GET /revision/flashcards — cards for the topics due or weak, writing any that are missing.
func (s *svc) flashcards(w http.ResponseWriter, r *http.Request) error {
	userID, ctx := auth.UserID(r), r.Context()
	today, _, err := s.learnerDay(ctx, userID)
	if err != nil {
		return err
	}
	cards := store.NewFlashcards(s.d.DB.Admin)
	topics, err := cards.RevisionTopics(ctx, userID, today, weakMastery, deckTopics)
	if err != nil {
		return err
	}
	ids := make([]string, len(topics))
	for i, t := range topics {
		ids[i] = t.TopicID
	}
	stored, err := cards.ForTopics(ctx, userID, ids)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, c := range stored {
		have[c.TopicID] = true
	}

	// Write the missing decks in parallel: a first visit waits for one model call, not four.
	// ponytail: two tabs opened at once can both write a topic's deck; a per-topic lock if it matters.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed error
	for _, t := range topics {
		if have[t.TopicID] {
			continue
		}
		wg.Go(func() {
			if err := s.writeCards(ctx, userID, t); err != nil {
				mu.Lock()
				failed = err
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if failed != nil {
		if stored, err = cards.ForTopics(ctx, userID, ids); err != nil {
			return err
		}
		if len(stored) == 0 {
			return failed // nothing to show: say why (no AI, AI busy)
		}
		slog.Warn("flashcards: some decks could not be written", "user", userID, "err", failed)
	} else if stored, err = cards.ForTopics(ctx, userID, ids); err != nil {
		return err
	}

	cite, err := s.citationsByChunk(ctx, userID, stored)
	if err != nil {
		return err
	}
	out := make([]deckTopic, 0, len(topics))
	for _, t := range topics {
		d := deckTopic{RevisionTopic: t, Cards: []cardView{}}
		for _, c := range stored {
			if c.TopicID != t.TopicID {
				continue
			}
			v := cardView{ID: c.ID, Front: c.Front, Back: c.Back, SourceChunkID: c.SourceChunkID, Citations: []ai.Citation{}}
			if c.SourceChunkID != nil {
				if ct, ok := cite[*c.SourceChunkID]; ok {
					v.Citations = []ai.Citation{ct}
				}
			}
			d.Cards = append(d.Cards, v)
		}
		if len(d.Cards) > 0 {
			out = append(out, d)
		}
	}
	httpx.OK(w, http.StatusOK, map[string]any{"today": today, "topics": out, "incomplete": failed != nil})
	return nil
}

// writeCards writes one topic's deck from its excerpts. The model numbers the excerpt each card
// came from; a card pointing at an excerpt it was not shown is dropped, so every card can cite.
func (s *svc) writeCards(ctx context.Context, userID string, t store.RevisionTopic) error {
	chunks, err := s.sources().ChunksForTopic(ctx, t.TopicID, userID, excerptsPerDeck)
	if err != nil || len(chunks) == 0 {
		return err
	}
	shown := make([]ai.PromptExcerpt, len(chunks))
	for i, c := range chunks {
		text := []rune(c.Content)
		shown[i] = ai.PromptExcerpt{N: i + 1, Text: string(text[:min(len(text), excerptChars)])}
	}
	var out struct {
		Cards []struct {
			Front         string `json:"front"`
			Back          string `json:"back"`
			SourceExcerpt int    `json:"sourceExcerpt"`
		} `json:"cards"`
	}
	if _, err := s.d.AI.JSON(ctx, ai.Request{System: ai.FlashcardSystem, Prompt: ai.FlashcardPrompt(t.Name, cardsPerTopic, shown),
		Schema: ai.FlashcardsSchema, Quality: ai.Standard, MaxTokens: 2000}, &out); err != nil {
		return err
	}
	var keep []store.NewFlashcard
	for _, c := range out.Cards {
		front, back := clip(c.Front, 500), clip(c.Back, 1000)
		if front == "" || back == "" || c.SourceExcerpt < 1 || c.SourceExcerpt > len(chunks) || len(keep) == cardsPerTopic {
			continue
		}
		keep = append(keep, store.NewFlashcard{Front: front, Back: back, SourceChunkID: chunks[c.SourceExcerpt-1].ID})
	}
	if len(keep) == 0 {
		return malformedMap("No usable flashcards came back. Try again.")
	}
	return store.NewFlashcards(s.d.DB.Admin).Insert(ctx, userID, t.TopicID, keep)
}

// citationsByChunk is a citation chip for each card's excerpt, keyed by chunk id.
func (s *svc) citationsByChunk(ctx context.Context, userID string, cards []store.FlashcardRow) (map[string]ai.Citation, error) {
	var ids []string
	for _, c := range cards {
		if c.SourceChunkID != nil {
			ids = append(ids, *c.SourceChunkID)
		}
	}
	chunks, err := s.sources().ChunksByIDs(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	sourceIDs := make([]string, len(chunks))
	for i, c := range chunks {
		sourceIDs[i] = c.SourceID
	}
	srcs, err := s.sources().FindMany(ctx, sourceIDs)
	if err != nil {
		return nil, err
	}
	byID := map[string]store.SourceRow{}
	for _, src := range srcs {
		byID[src.ID] = src
	}
	out := map[string]ai.Citation{}
	for _, c := range chunks {
		src := byID[c.SourceID]
		out[c.ID] = ai.Citation{N: 1, ChunkID: c.ID, SourceID: c.SourceID, SourceTitle: src.Title, Kind: src.Kind,
			Modality: c.Modality, Location: locationOf(c), Excerpt: c.Content, TopicID: c.TopicID}
	}
	return out, nil
}

// POST /revision/flashcards/{id}/review {known}: "known" or "again" moves the topic's schedule.
func (s *svc) reviewFlashcard(w http.ResponseWriter, r *http.Request) error {
	id, err := uuidParam(r, "id")
	if err != nil {
		return err
	}
	var body struct {
		Known *bool `json:"known"`
	}
	if err := httpx.Decode(w, r, &body); err != nil {
		return err
	}
	if body.Known == nil {
		return invalid("known: Invalid input: expected boolean, received undefined")
	}
	userID, ctx := auth.UserID(r), r.Context()
	card, err := store.NewFlashcards(s.d.DB.Admin).Find(ctx, id, userID)
	if err != nil {
		return err
	}
	today, daysUntilExam, err := s.learnerDay(ctx, userID)
	if err != nil {
		return err
	}
	rev := store.NewRevisions(s.d.DB.Admin)
	state, err := rev.FindState(ctx, userID, card.TopicID)
	if err != nil {
		return err
	}
	update := learner.FlashcardReview(state, *body.Known, today, daysUntilExam)
	if update != nil {
		if err := rev.Save(ctx, userID, card.TopicID, *update); err != nil {
			return err
		}
	}
	httpx.OK(w, http.StatusOK, map[string]any{"topicId": card.TopicID, "moved": update != nil, "schedule": update})
	return nil
}
