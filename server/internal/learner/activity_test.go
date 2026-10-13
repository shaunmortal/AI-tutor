package learner

import (
	"github.com/shaunmortal/AI-tutor/server/internal/dates"

	"slices"
	"strings"
	"testing"
)

// The streaks are computed here rather than read from learner_profiles, so these tests
// stand behind the number a learner sees next to a year of squares.

const calToday = "2026-08-14"

// counts gives every date the same number of answers.
func counts(dates []string, perDay int) map[string]int {
	m := map[string]int{}
	for _, d := range dates {
		m[d] = perDay
	}
	return m
}

// run is n consecutive dates ending on end.
func run(end string, n int) []string {
	out := make([]string, n)
	for i := range n {
		out[i] = dates.AddDays(end, -(n - 1 - i))
	}
	return out
}

func TestLevelFor(t *testing.T) {
	// The top shade is reachable on a genuinely good day: ten questions, not fifty.
	for _, c := range []struct{ count, want int }{{0, 0}, {1, 1}, {5, 2}, {8, 3}, {10, 4}, {200, 4}} {
		if got := LevelFor(c.count); got != c.want {
			t.Errorf("LevelFor(%d) = %d, want %d", c.count, got, c.want)
		}
	}
	// Never a shade the client has no colour for.
	for _, count := range []int{0, 1, 2, 3, 6, 9, 10, 11, 1000} {
		if got := LevelFor(count); got < 0 || got > 4 {
			t.Errorf("LevelFor(%d) = %d", count, got)
		}
	}
}

func TestCalendarGrid(t *testing.T) {
	t.Run("fixed-size window ending today", func(t *testing.T) {
		cal := BuildCalendar(map[string]int{}, calToday)
		if len(cal.Days) != CalendarDays || cal.To != calToday ||
			cal.Days[len(cal.Days)-1].Date != calToday || cal.Days[0].Date != cal.From {
			t.Fatalf("len %d, from %s, to %s", len(cal.Days), cal.From, cal.To)
		}
	})

	t.Run("includes empty days", func(t *testing.T) {
		cal := BuildCalendar(counts([]string{calToday}, 3), calToday)
		empty := 0
		for _, d := range cal.Days {
			if d.Count == 0 {
				empty++
			}
		}
		if empty != CalendarDays-1 {
			t.Fatalf("empty days %d", empty)
		}
	})

	t.Run("days in ascending order", func(t *testing.T) {
		cal := BuildCalendar(counts(run(calToday, 10), 3), calToday)
		if !slices.IsSortedFunc(cal.Days, func(a, b ActivityDay) int { return strings.Compare(a.Date, b.Date) }) {
			t.Fatal("days out of order")
		}
	})

	t.Run("ignores activity older than the window", func(t *testing.T) {
		cal := BuildCalendar(counts([]string{"2020-01-01", "2019-06-06", calToday}, 3), calToday)
		if cal.ActiveDays != 1 || cal.TotalAnswered != 3 {
			t.Fatalf("active %d, total %d", cal.ActiveDays, cal.TotalAnswered)
		}
	})

	t.Run("counts totals and active days separately", func(t *testing.T) {
		// Twelve answers over three days is three squares, not twelve.
		cal := BuildCalendar(counts(run(calToday, 3), 4), calToday)
		if cal.ActiveDays != 3 || cal.TotalAnswered != 12 {
			t.Fatalf("active %d, total %d", cal.ActiveDays, cal.TotalAnswered)
		}
	})
}

func TestCalendarMaxStreak(t *testing.T) {
	for _, c := range []struct {
		name  string
		dates []string
		want  int
	}{
		{"longest run anywhere in the year", slices.Concat(run("2026-03-20", 14), run("2026-05-10", 5), run(calToday, 2)), 14},
		{"zero with no answers", nil, 0},
		{"does not join runs across a gap", slices.Concat(run("2026-07-01", 4), run("2026-07-10", 4)), 4},
	} {
		if got := BuildCalendar(counts(c.dates, 3), calToday).MaxStreak; got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}

	// An unbroken year must not run past the window.
	cal := BuildCalendar(counts(run(calToday, CalendarDays), 3), calToday)
	if cal.MaxStreak != CalendarDays || cal.CurrentStreak != CalendarDays {
		t.Fatalf("max %d, current %d", cal.MaxStreak, cal.CurrentStreak)
	}
}

func TestCalendarCurrentStreak(t *testing.T) {
	for _, c := range []struct {
		name  string
		dates []string
		want  int
	}{
		{"run ending today", run(calToday, 6), 6},
		// Somebody opening the app at nine in the morning has not lost their streak yet.
		{"today not started yet", run(dates.AddDays(calToday, -1), 5), 5},
		// Last active the day before yesterday: that gap is real.
		{"a day actually missed", run(dates.AddDays(calToday, -2), 9), 0},
		{"no answers", nil, 0},
	} {
		if got := BuildCalendar(counts(c.dates, 3), calToday).CurrentStreak; got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}

	// The current run is one of the runs, so it can never exceed the max.
	for _, dates := range [][]string{
		run(calToday, 7),
		slices.Concat(run("2026-04-01", 20), run(calToday, 3)),
		run(dates.AddDays(calToday, -1), 4),
		nil,
	} {
		cal := BuildCalendar(counts(dates, 3), calToday)
		if cal.CurrentStreak > cal.MaxStreak {
			t.Errorf("current %d > max %d", cal.CurrentStreak, cal.MaxStreak)
		}
	}
}
