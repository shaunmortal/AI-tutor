package learner

import (
	"github.com/shaunmortal/AI-tutor/server/internal/dates"

	"math"
	"testing"
)

// Streaks are the number a learner checks every day; somebody told their two-day streak
// is one will not report a bug, they will stop believing the screen. So these are the
// boundaries: same day twice, one day's gap, two days', and a clock that disagrees.

var noTiming = HabitEvent{Today: "2026-08-13"}

func TestDaysBetween(t *testing.T) {
	for _, c := range []struct {
		from, to string
		want     int
	}{
		{"2026-08-12", "2026-08-13", 1},
		{"2026-07-31", "2026-08-01", 1},
		{"2025-12-31", "2026-01-01", 1},
		{"2026-08-13", "2026-08-13", 0},
		// Negative when the dates are the wrong way round.
		{"2026-08-13", "2026-08-12", -1},
	} {
		if got, ok := dates.DaysBetween(c.from, c.to); !ok || got != c.want {
			t.Errorf("dates.DaysBetween(%s, %s) = %d, %v; want %d", c.from, c.to, got, ok, c.want)
		}
	}
}

func TestNextHabitsStreaks(t *testing.T) {
	for _, c := range []struct {
		name                     string
		current, longest         int
		last                     *string
		wantCurrent, wantLongest int
	}{
		{"starts at one on the first answer", 0, 0, nil, 1, 1},
		// The record stands until it is actually beaten.
		{"extends after yesterday", 4, 9, ptr("2026-08-12"), 5, 9},
		// Twenty questions in one day is one day.
		{"same day counts once", 4, 4, ptr("2026-08-13"), 4, 4},
		// Broken, not erased: the twelve days happened.
		{"breaks after a missed day", 12, 12, ptr("2026-08-11"), 1, 12},
		{"raises the record when passed", 9, 9, ptr("2026-08-12"), 10, 10},
		// A wrong device clock is the app's mistake, not the learner's.
		{"future last date leaves it alone", 6, 6, ptr("2026-08-20"), 6, 6},
	} {
		h := NextHabits(HabitState{CurrentStreakDays: c.current, LongestStreakDays: c.longest, LastActivityDate: c.last}, noTiming)
		if h.CurrentStreakDays != c.wantCurrent || h.LongestStreakDays != c.wantLongest || h.LastActivityDate != "2026-08-13" {
			t.Errorf("%s: got %+v", c.name, h)
		}
	}

	// The database CHECKs current <= longest, or the write is rejected.
	for _, s := range []HabitState{
		{},
		{CurrentStreakDays: 3, LongestStreakDays: 3, LastActivityDate: ptr("2026-08-12")},
		{CurrentStreakDays: 7, LongestStreakDays: 2, LastActivityDate: ptr("2026-08-12")},
		{CurrentStreakDays: 1, LongestStreakDays: 40, LastActivityDate: ptr("2026-01-01")},
	} {
		if h := NextHabits(s, noTiming); h.CurrentStreakDays > h.LongestStreakDays {
			t.Errorf("%+v -> %+v", s, h)
		}
	}
}

func TestNextHabitsSpeed(t *testing.T) {
	timed := func(seconds float64) HabitEvent { return HabitEvent{Today: "2026-08-13", SecondsTaken: &seconds} }

	if h := NextHabits(HabitState{}, timed(30)); h.AvgSecondsPerQuestion == nil || *h.AvgSecondsPerQuestion != 30 {
		t.Errorf("first timed answer: %v", h.AvgSecondsPerQuestion)
	}

	// 20 answers averaging 30s, then one at 60s: (600 + 60) / 21. toBeCloseTo(31.43, 2).
	h := NextHabits(HabitState{AvgSecondsPerQuestion: ptr(30.0), TotalAttempts: 20}, timed(60))
	if h.AvgSecondsPerQuestion == nil || math.Abs(*h.AvgSecondsPerQuestion-31.43) >= 0.005 {
		t.Errorf("running mean: %v", h.AvgSecondsPerQuestion)
	}

	// Came back an hour later: credited at the 600s cap, within the database's 0-3600 CHECK.
	h = NextHabits(HabitState{AvgSecondsPerQuestion: ptr(30.0), TotalAttempts: 9}, timed(3600))
	if h.AvgSecondsPerQuestion == nil || *h.AvgSecondsPerQuestion != 87 || *h.AvgSecondsPerQuestion > 3600 {
		t.Errorf("capped: %v", h.AvgSecondsPerQuestion)
	}

	h = NextHabits(HabitState{AvgSecondsPerQuestion: ptr(42.0), TotalAttempts: 5}, noTiming)
	if h.AvgSecondsPerQuestion == nil || *h.AvgSecondsPerQuestion != 42 {
		t.Errorf("untimed: %v", h.AvgSecondsPerQuestion)
	}
}

func TestConsistencyPercent(t *testing.T) {
	for _, c := range []struct {
		name string
		span PracticeSpan
		want *float64
	}{
		// The span includes today: 1/1, not a division by zero.
		{"first day scores 100", PracticeSpan{1, "2026-08-13"}, ptr(100.0)},
		// Started 9 days ago, studied 5 of the 10 days including today.
		{"days studied over days available", PracticeSpan{5, "2026-08-04"}, ptr(50.0)},
		// Client and server in different timezones can record two days for one.
		{"never above 100", PracticeSpan{5, "2026-08-12"}, ptr(100.0)},
		{"nil with nothing to measure", PracticeSpan{0, "2026-08-13"}, nil},
	} {
		got := ConsistencyPercent(c.span, "2026-08-13")
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
