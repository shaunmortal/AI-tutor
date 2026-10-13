package learner

import "testing"

func TestFlashcardReview(t *testing.T) {
	today := "2026-10-13"
	yesterday := "2026-10-12"

	// No schedule yet, known: a gentle step from the first interval, not a full pass.
	u := FlashcardReview(nil, true, today, nil)
	if u == nil || u.IntervalDays != 1 || u.DueOn != "2026-10-14" || u.Lapsed {
		t.Fatalf("first known: %+v", u)
	}

	// Again: collapses to tomorrow and counts a lapse.
	state := &ReviewState{IntervalDays: 10, EaseFactor: 2.5, ReviewCount: 3, LastReviewedOn: &yesterday}
	u = FlashcardReview(state, false, today, nil)
	if u == nil || u.IntervalDays != 1 || !u.Lapsed || u.Lapses != 1 {
		t.Fatalf("again: %+v", u)
	}

	// Known: grows by the shaky multiplier.
	u = FlashcardReview(state, true, today, nil)
	if u == nil || u.IntervalDays != 12 {
		t.Fatalf("known: %+v", u)
	}

	// Already reviewed today: known changes nothing; again still collapses a long interval...
	reviewedToday := &ReviewState{IntervalDays: 10, EaseFactor: 2.5, ReviewCount: 3, LastReviewedOn: &today}
	if u := FlashcardReview(reviewedToday, true, today, nil); u != nil {
		t.Fatalf("second known today moved the schedule: %+v", u)
	}
	if u := FlashcardReview(reviewedToday, false, today, nil); u == nil || u.IntervalDays != 1 {
		t.Fatalf("again today: %+v", u)
	}
	// ...but not one already at a day.
	collapsed := &ReviewState{IntervalDays: 1, EaseFactor: 2.35, ReviewCount: 4, Lapses: 1, LastReviewedOn: &today}
	if u := FlashcardReview(collapsed, false, today, nil); u != nil {
		t.Fatalf("collapsed twice in a day: %+v", u)
	}

	// The exam caps the interval.
	days := 3
	if u := FlashcardReview(&ReviewState{IntervalDays: 30, EaseFactor: 2.5, ReviewCount: 5, LastReviewedOn: &yesterday}, true, today, &days); u == nil || u.IntervalDays != 2 {
		t.Fatalf("exam cap: %+v", u)
	}
}

func TestSelfRatedPrior(t *testing.T) {
	for in, want := range map[int]float64{0: 15, 1: 15, 3: 35, 5: 65, 9: 65} {
		if got := SelfRatedPrior(in); got != want {
			t.Errorf("SelfRatedPrior(%d) = %v, want %v", in, got, want)
		}
	}
}
