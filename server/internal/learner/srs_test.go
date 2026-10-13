package learner

import (
	"github.com/shaunmortal/AI-tutor/server/internal/dates"

	"slices"
	"testing"
)

// Getting the scheduler wrong is quiet: intervals that grow too fast let a topic be
// forgotten unannounced, too slow and the plan fills with things already known. Neither
// shows as an error, so the boundaries are tested directly, the exam ceiling most of all
// because it is the part that is not SM-2.

const srsToday = "2026-08-14"

func reviewState(mod func(*ReviewState)) ReviewState {
	s := ReviewState{IntervalDays: 3, EaseFactor: 2.5, ReviewCount: 1, LastReviewedOn: ptr("2026-08-11")}
	if mod != nil {
		mod(&s)
	}
	return s
}

func reviewOutcome(accuracy float64, mod func(*ReviewOutcome)) ReviewOutcome {
	o := ReviewOutcome{ReviewedOn: srsToday, AccuracyPercent: accuracy, QuestionsAnswered: 5, DaysUntilExam: ptr(180)}
	if mod != nil {
		mod(&o)
	}
	return o
}

func examIn(days int) func(*ReviewOutcome) {
	return func(o *ReviewOutcome) { o.DaysUntilExam = &days }
}

// stateAfter carries a schedule update forward as the next review's state.
func stateAfter(u ScheduleUpdate) ReviewState {
	return ReviewState{u.IntervalDays, u.EaseFactor, u.ReviewCount, u.Lapses, &u.LastReviewedOn}
}

func TestAddDays(t *testing.T) {
	for _, c := range []struct {
		date string
		days int
		want string
	}{
		{"2026-08-30", 3, "2026-09-02"},
		{"2026-12-30", 3, "2027-01-02"},
		{"2026-03-01", -1, "2026-02-28"},
		{"2026-01-01", -1, "2025-12-31"},
	} {
		if got := dates.AddDays(c.date, c.days); got != c.want {
			t.Errorf("dates.AddDays(%s, %d) = %s, want %s", c.date, c.days, got, c.want)
		}
	}
}

func TestDaysOverdueAndIsDue(t *testing.T) {
	for _, c := range []struct {
		due     string
		overdue int
		isDue   bool
	}{
		{"2026-08-10", 4, true},
		// Due "today" has to mean now, not tomorrow, or every topic is a day late.
		{srsToday, 0, true},
		{"2026-08-20", -6, false},
	} {
		if got := DaysOverdue(c.due, srsToday); got != c.overdue {
			t.Errorf("DaysOverdue(%s) = %d, want %d", c.due, got, c.overdue)
		}
		if got := IsDue(c.due, srsToday); got != c.isDue {
			t.Errorf("IsDue(%s) = %v, want %v", c.due, got, c.isDue)
		}
	}
}

func TestFirstSchedule(t *testing.T) {
	// The first forgetting happens fastest, so the first gap is the shortest.
	first := FirstSchedule(srsToday, ptr(180))
	if first.DueOn != "2026-08-15" || first.IntervalDays != 1 || first.ReviewCount != 0 || first.FinishedForThisGoal {
		t.Fatalf("%+v", first)
	}

	// No review is scheduled that the learner has no time for.
	if !FirstSchedule(srsToday, ptr(1)).FinishedForThisGoal {
		t.Fatal("expected finished with the exam tomorrow")
	}
}

func TestNextReviewPassing(t *testing.T) {
	t.Run("fixed second gap rather than multiplying immediately", func(t *testing.T) {
		s := reviewState(func(s *ReviewState) { s.IntervalDays, s.ReviewCount = 1, 0 })
		next := NextReview(&s, reviewOutcome(100, nil))
		if next.IntervalDays != SecondIntervalDays || next.ReviewCount != 1 || next.Lapsed {
			t.Fatalf("%+v", next)
		}
	})

	t.Run("multiplies by ease from the third review", func(t *testing.T) {
		s := reviewState(func(s *ReviewState) { s.IntervalDays, s.EaseFactor, s.ReviewCount = 6, 2.5, 2 })
		next := NextReview(&s, reviewOutcome(100, nil))
		if next.IntervalDays != 15 || next.DueOn != dates.AddDays(srsToday, 15) {
			t.Fatalf("%+v", next)
		}
	})

	t.Run("raises ease when reviews keep going well", func(t *testing.T) {
		s := reviewState(func(s *ReviewState) { s.ReviewCount, s.EaseFactor = 3, 2.5 })
		next := NextReview(&s, reviewOutcome(100, nil))
		if next.EaseFactor <= 2.5 || next.EaseFactor > MaxEase {
			t.Fatalf("ease %v", next.EaseFactor)
		}
	})

	t.Run("never lets ease run away", func(t *testing.T) {
		current := reviewState(func(s *ReviewState) { s.ReviewCount, s.EaseFactor = 5, 2.95 })
		for range 10 {
			current = stateAfter(NextReview(&current, reviewOutcome(100, nil)))
		}
		if current.EaseFactor > MaxEase {
			t.Fatalf("ease %v", current.EaseFactor)
		}
	})

	t.Run("caps the interval so a topic is revised, not parked", func(t *testing.T) {
		s := reviewState(func(s *ReviewState) { s.IntervalDays, s.EaseFactor, s.ReviewCount = 55, 2.8, 6 })
		if next := NextReview(&s, reviewOutcome(100, examIn(365))); next.IntervalDays > MaxIntervalDays {
			t.Fatalf("interval %d", next.IntervalDays)
		}
	})
}

func TestNextReviewFailing(t *testing.T) {
	t.Run("collapses to tomorrow and counts a lapse", func(t *testing.T) {
		s := reviewState(func(s *ReviewState) { s.IntervalDays, s.ReviewCount, s.Lapses = 30, 4, 1 })
		next := NextReview(&s, reviewOutcome(20, nil))
		if next.IntervalDays != 1 || next.DueOn != dates.AddDays(srsToday, 1) || !next.Lapsed || next.Lapses != 2 {
			t.Fatalf("%+v", next)
		}
	})

	t.Run("lowers ease so a hard topic comes back sooner", func(t *testing.T) {
		s := reviewState(nil)
		if next := NextReview(&s, reviewOutcome(20, nil)); next.EaseFactor >= 2.5 {
			t.Fatalf("ease %v", next.EaseFactor)
		}
	})

	t.Run("never lets ease fall below the floor", func(t *testing.T) {
		current := reviewState(func(s *ReviewState) { s.EaseFactor, s.ReviewCount, s.Lapses = 1.4, 5, 2 })
		for range 10 {
			current = stateAfter(NextReview(&current, reviewOutcome(10, nil)))
		}
		if current.EaseFactor < MinEase {
			t.Fatalf("ease %v", current.EaseFactor)
		}
	})

	t.Run("keeps lapses within the review count, as the database requires", func(t *testing.T) {
		current := reviewState(func(s *ReviewState) { s.ReviewCount, s.Lapses = 0, 0 })
		for range 6 {
			next := NextReview(&current, reviewOutcome(10, nil))
			if next.Lapses > next.ReviewCount {
				t.Fatalf("%+v", next)
			}
			current = stateAfter(next)
		}
	})
}

func TestNextReviewShakyMiddle(t *testing.T) {
	// Neither a reset nor a full step is honest about 65%.
	s := reviewState(func(s *ReviewState) { s.IntervalDays, s.ReviewCount = 10, 3 })
	next := NextReview(&s, reviewOutcome(65, nil))
	if next.IntervalDays <= 10 || next.IntervalDays >= 25 || next.Lapsed {
		t.Fatalf("%+v", next)
	}

	s = reviewState(nil)
	if next := NextReview(&s, reviewOutcome(65, nil)); next.EaseFactor != 2.5 {
		t.Fatalf("ease %v", next.EaseFactor)
	}
}

func TestNextReviewThinEvidence(t *testing.T) {
	s := reviewState(func(s *ReviewState) { s.IntervalDays, s.ReviewCount, s.EaseFactor = 10, 3, 2.5 })
	generous := NextReview(&s, reviewOutcome(100, func(o *ReviewOutcome) { o.QuestionsAnswered = 5 }))
	thin := NextReview(&s, reviewOutcome(100, func(o *ReviewOutcome) { o.QuestionsAnswered = 2 }))
	// Two out of two is encouraging, not proof.
	if thin.IntervalDays >= generous.IntervalDays {
		t.Fatalf("thin %d, generous %d", thin.IntervalDays, generous.IntervalDays)
	}

	// Getting one of two wrong is still getting it wrong.
	s = reviewState(nil)
	next := NextReview(&s, reviewOutcome(0, func(o *ReviewOutcome) { o.QuestionsAnswered = 1 }))
	if next.EaseFactor >= 2.5 || !next.Lapsed {
		t.Fatalf("%+v", next)
	}
}

func TestNextReviewExamCeiling(t *testing.T) {
	t.Run("never schedules a review after the exam", func(t *testing.T) {
		s := reviewState(func(s *ReviewState) { s.IntervalDays, s.EaseFactor, s.ReviewCount = 20, 2.5, 4 })
		if next := NextReview(&s, reviewOutcome(100, examIn(10))); next.DueOn > dates.AddDays(srsToday, 10) {
			t.Fatalf("due %s", next.DueOn)
		}
	})

	t.Run("shortens the last review rather than dropping it", func(t *testing.T) {
		s := reviewState(func(s *ReviewState) { s.IntervalDays, s.EaseFactor, s.ReviewCount = 30, 2.5, 4 })
		next := NextReview(&s, reviewOutcome(100, examIn(6)))
		if next.IntervalDays >= 30 || next.IntervalDays <= 0 || next.FinishedForThisGoal {
			t.Fatalf("%+v", next)
		}
	})

	t.Run("finished when no review fits", func(t *testing.T) {
		s := reviewState(nil)
		if !NextReview(&s, reviewOutcome(100, examIn(1))).FinishedForThisGoal {
			t.Fatal("expected finished")
		}
	})

	t.Run("leaves the exam morning alone", func(t *testing.T) {
		s := reviewState(nil)
		if next := NextReview(&s, reviewOutcome(100, examIn(2))); next.DueOn == dates.AddDays(srsToday, 2) {
			t.Fatalf("due on the exam morning %s", next.DueOn)
		}
	})

	t.Run("ordinary ceiling with no goal", func(t *testing.T) {
		s := reviewState(func(s *ReviewState) { s.IntervalDays, s.ReviewCount = 20, 4 })
		next := NextReview(&s, reviewOutcome(100, func(o *ReviewOutcome) { o.DaysUntilExam = nil }))
		if next.IntervalDays <= 20 || next.IntervalDays > MaxIntervalDays || next.FinishedForThisGoal {
			t.Fatalf("%+v", next)
		}
	})
}

func TestNextReviewWithoutState(t *testing.T) {
	// Every topic practised before spaced repetition shipped is in this state.
	next := NextReview(nil, reviewOutcome(90, nil))
	if next.ReviewCount != 1 || next.IntervalDays < 1 || next.DueOn <= srsToday {
		t.Fatalf("%+v", next)
	}
}

func TestRealisticIntervals(t *testing.T) {
	var current *ReviewState
	day := srsToday
	var seen []int

	for range 6 {
		next := NextReview(current, reviewOutcome(100, func(o *ReviewOutcome) { o.ReviewedOn = day }))
		seen = append(seen, next.IntervalDays)
		s := stateAfter(next)
		current = &s
		day = next.DueOn
	}

	// Recognisably SM-2: short opening steps, then compounding, then the ceiling.
	if seen[0] != 3 || !slices.IsSorted(seen) || seen[len(seen)-1] > MaxIntervalDays {
		t.Fatalf("intervals %v", seen)
	}
}
