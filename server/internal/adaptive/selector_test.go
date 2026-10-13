package adaptive

import (
	"context"
	"slices"
	"testing"
)

// fakeBank mimics the repository (ignoring topic): difficulty, exclude, only, limit.
type fakeBank struct {
	questions []PracticeQuestion
	attempted []string
}

func (b fakeBank) FindAttemptedIDs(context.Context, string, string) ([]string, error) {
	return b.attempted, nil
}

func (b fakeBank) FindForPractice(_ context.Context, q PracticeQuery) ([]PracticeQuestion, error) {
	out := []PracticeQuestion{}
	for _, x := range b.questions {
		if (q.Difficulty != "" && x.Difficulty != q.Difficulty) || slices.Contains(q.Exclude, x.ID) ||
			(q.Only != nil && !slices.Contains(q.Only, x.ID)) || len(out) >= q.Limit {
			continue
		}
		out = append(out, x)
	}
	return out, nil
}

func ids(qs []PracticeQuestion) []string {
	out := []string{}
	for _, q := range qs {
		out = append(out, q.ID)
	}
	return out
}

func TestSelectQuestionsFallbacks(t *testing.T) {
	bank := fakeBank{
		questions: []PracticeQuestion{
			{ID: "e1", Difficulty: Easy}, {ID: "m1", Difficulty: Medium}, {ID: "h1", Difficulty: Hard},
			{ID: "e2", Difficulty: Easy}, {ID: "e3", Difficulty: Easy},
		},
		attempted: []string{"e2", "e3"}, // e2 answered most recently
	}

	// Unseen easy, then nearest level (medium before hard), then repeats oldest first.
	got, err := SelectQuestions(context.Background(), bank, SelectOptions{Difficulty: Easy, Count: 4})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"e1", "m1", "h1", "e3"}; !slices.Equal(ids(got.Questions), want) {
		t.Fatalf("questions = %v, want %v", ids(got.Questions), want)
	}
	if got.Repeats != 1 || got.Short || got.Note == nil || *got.Note != "You have seen every question on this topic, so some are repeats." {
		t.Fatalf("got %+v", got)
	}

	// Asking for more than exists is reported as short.
	got, _ = SelectQuestions(context.Background(), bank, SelectOptions{Difficulty: Hard, Count: 9})
	if !got.Short || len(got.Questions) != 5 || got.Repeats != 2 {
		t.Fatalf("got %+v", got)
	}
}
