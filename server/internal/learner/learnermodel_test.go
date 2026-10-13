package learner

import (
	"context"
	"testing"
	"time"
)

type fakeSource struct {
	profile *ProfileRow
	mastery []TopicMasteryRow
	limit   int
}

func (f *fakeSource) FindProfile(context.Context, string) (*ProfileRow, error) { return f.profile, nil }
func (f *fakeSource) FindMastery(_ context.Context, _ string, limit int) ([]TopicMasteryRow, error) {
	f.limit = limit
	return f.mastery, nil
}

func TestGetSummaryEmpty(t *testing.T) {
	s, err := NewModelService(&fakeSource{}).GetSummary(context.Background(), "u")
	if err != nil || s.HasActivity || s.Attempts.Total != 0 || s.ComputedAt != nil || s.WeakestTopics == nil || len(s.WeakestTopics) != 0 {
		t.Fatalf("empty summary %+v %v", s, err)
	}
}

func TestGetSummary(t *testing.T) {
	at := time.Now()
	src := &fakeSource{
		profile: &ProfileRow{TotalAttempts: 4, CorrectAttempts: 3, OverallAccuracy: ptr(75.0), CurrentStreakDays: 2,
			LongestStreakDays: 5, LastActivityDate: ptr("2026-10-13"), PreferredDifficulty: ptr("medium"), ComputedAt: at},
		mastery: []TopicMasteryRow{
			{TopicID: "a", MasteryScore: 20, TotalAttempts: 3, Topics: &TopicRef{Name: "Joins"}},
			{TopicID: "b", MasteryScore: 40, TotalAttempts: 1, Accuracy: ptr(100.0)},
		},
	}
	s, err := NewModelService(src).GetSummary(context.Background(), "u")
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasActivity || s.Attempts.Correct != 3 || *s.Attempts.AccuracyPercent != 75 || s.Habits.LongestStreakDays != 5 ||
		*s.PreferredDifficulty != "medium" || !s.ComputedAt.Equal(at) || src.limit != weakTopicLimit {
		t.Errorf("summary %+v", s)
	}
	if len(s.WeakestTopics) != 2 || s.WeakestTopics[0].Name != "Joins" || s.WeakestTopics[1].Name != "Unknown topic" ||
		*s.WeakestTopics[1].AccuracyPercent != 100 {
		t.Errorf("weakest %+v", s.WeakestTopics)
	}
}
