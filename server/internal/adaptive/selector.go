package adaptive

import (
	"context"
	"slices"
)

// PracticeQuestion is a question as the learner may see it: no answer, no explanation.
// The snake_case tags match the columns the frontend already reads.
type PracticeQuestion struct {
	ID           string     `json:"id"`
	TopicID      string     `json:"topic_id"`
	QuestionType string     `json:"question_type"`
	Body         string     `json:"body"`
	Options      []string   `json:"options"`
	Difficulty   Difficulty `json:"difficulty"`
	Marks        float64    `json:"marks"`
}

// PracticeQuery asks the bank for questions on one topic.
type PracticeQuery struct {
	TopicID string
	// Empty means any difficulty.
	Difficulty Difficulty
	Exclude    []string
	// When non-nil, restrict to these ids (an empty, non-nil slice matches nothing).
	Only  []string
	Limit int
}

// QuestionBank is the slice of the question repository the selector needs.
type QuestionBank interface {
	// FindAttemptedIDs returns question ids the learner has attempted on a topic, most
	// recently answered first, each once.
	FindAttemptedIDs(ctx context.Context, userID, topicID string) ([]string, error)
	FindForPractice(ctx context.Context, q PracticeQuery) ([]PracticeQuestion, error)
}

// SelectedQuestions is what the selector could find.
type SelectedQuestions struct {
	Questions []PracticeQuestion `json:"questions"`
	// True when the bank could not fill the request, so the UI can say so honestly.
	Short bool `json:"short"`
	// Which fallback was needed, if any. Useful when a topic looks thin.
	Note *string `json:"note"`
	// How many of the questions are ones the learner has answered before.
	Repeats int `json:"repeats"`
}

// SelectOptions is the request: count questions at difficulty on a topic.
type SelectOptions struct {
	UserID     string
	TopicID    string
	Difficulty Difficulty
	Count      int
}

// Levels in order of closeness to the one asked for, nearest first.
var nearest = map[Difficulty][]Difficulty{
	Easy:   {Medium, Hard},
	Medium: {Easy, Hard},
	Hard:   {Medium, Easy},
}

// SelectQuestions turns "5 medium questions on Trees" into actual questions.
//
// Three tries, in order of preference:
//  1. unseen questions at the difficulty the engine asked for
//  2. unseen questions at the nearest other difficulty — medium before hard for a learner
//     who asked for easy, so a struggling learner is not handed the hardest questions first
//  3. questions the learner has seen before, the ones answered longest ago first
//
// Repeating is a real fallback rather than a failure -- it is how spaced repetition works --
// but it is reported, because a topic that always falls through to step 3 needs more
// questions in the bank. Repeats come oldest first: handing back what was answered a minute
// ago tests short-term memory, not learning.
func SelectQuestions(ctx context.Context, bank QuestionBank, o SelectOptions) (SelectedQuestions, error) {
	// Most recently answered first.
	attempted, err := bank.FindAttemptedIDs(ctx, o.UserID, o.TopicID)
	if err != nil {
		return SelectedQuestions{}, err
	}

	chosen, err := bank.FindForPractice(ctx, PracticeQuery{
		TopicID: o.TopicID, Difficulty: o.Difficulty, Exclude: attempted, Limit: o.Count,
	})
	if err != nil {
		return SelectedQuestions{}, err
	}

	var note *string
	repeats := 0
	setNote := func(s string) { note = &s }

	for _, level := range nearest[o.Difficulty] {
		if len(chosen) >= o.Count {
			break
		}
		exclude := slices.Clone(attempted)
		for _, q := range chosen {
			exclude = append(exclude, q.ID)
		}
		nearby, err := bank.FindForPractice(ctx, PracticeQuery{
			TopicID: o.TopicID, Difficulty: level, Exclude: exclude, Limit: o.Count - len(chosen),
		})
		if err != nil {
			return SelectedQuestions{}, err
		}
		if len(nearby) > 0 {
			setNote("Not enough unseen " + string(o.Difficulty) + " questions, so some are from other levels.")
			chosen = append(chosen, nearby...)
		}
	}

	if len(chosen) < o.Count && len(attempted) > 0 {
		taken := map[string]bool{}
		for _, q := range chosen {
			taken[q.ID] = true
		}
		oldestFirst := []string{}
		for _, id := range slices.Backward(attempted) {
			if !taken[id] {
				oldestFirst = append(oldestFirst, id)
			}
		}

		seen, err := bank.FindForPractice(ctx, PracticeQuery{
			TopicID: o.TopicID, Exclude: []string{}, Only: oldestFirst, Limit: len(oldestFirst),
		})
		if err != nil {
			return SelectedQuestions{}, err
		}
		byID := map[string]PracticeQuestion{}
		for _, q := range seen {
			byID[q.ID] = q
		}
		var seenAgain []PracticeQuestion
		for _, id := range oldestFirst {
			if q, ok := byID[id]; ok && len(seenAgain) < o.Count-len(chosen) {
				seenAgain = append(seenAgain, q)
			}
		}

		if len(seenAgain) > 0 {
			setNote("You have seen every question on this topic, so some are repeats.")
			chosen = append(chosen, seenAgain...)
			repeats = len(seenAgain)
		}
	}

	if chosen == nil {
		chosen = []PracticeQuestion{}
	}
	return SelectedQuestions{
		Questions: chosen[:min(len(chosen), o.Count)],
		Short:     len(chosen) < o.Count,
		Note:      note,
		Repeats:   repeats,
	}, nil
}
