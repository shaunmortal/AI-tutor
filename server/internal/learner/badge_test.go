package learner

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Badges are recomputed on every read, so these protect that the recompute cannot award
// something unearned or lose something earned.

func ids(badges []Badge) []string {
	out := []string{}
	for _, b := range badges {
		out = append(out, b.ID)
	}
	return out
}

func TestEarnedBadges(t *testing.T) {
	t.Run("nothing for a brand new learner", func(t *testing.T) {
		got := EarnedBadges(BadgeInputs{})
		if got == nil || len(got) != 0 {
			t.Fatalf("%#v", got)
		}
	})

	t.Run("tier only once its target is met", func(t *testing.T) {
		under := EarnedBadges(BadgeInputs{MaxStreak: 6, ActiveDates: run("2026-08-14", 6)})
		over := EarnedBadges(BadgeInputs{MaxStreak: 7, ActiveDates: run("2026-08-14", 7)})
		if slices.Contains(ids(under), "streak-7") || !slices.Contains(ids(over), "streak-7") {
			t.Fatalf("under %v, over %v", ids(under), ids(over))
		}
	})

	t.Run("no accuracy badge on too little evidence", func(t *testing.T) {
		// Four right out of four is a small sample, and a badge won in a minute means nothing.
		for _, b := range EarnedBadges(BadgeInputs{TotalAnswered: 4, AccuracyPercent: ptr(100.0)}) {
			if b.Family == FamilyAccuracy {
				t.Fatalf("awarded %s", b.ID)
			}
		}
	})

	t.Run("accuracy once there is enough behind it", func(t *testing.T) {
		got := ids(EarnedBadges(BadgeInputs{TotalAnswered: 40, AccuracyPercent: ptr(88.0)}))
		if !slices.Contains(got, "accuracy-70") || !slices.Contains(got, "accuracy-85") {
			t.Fatalf("%v", got)
		}
	})

	t.Run("every lower tier along with the one reached", func(t *testing.T) {
		got := ids(EarnedBadges(BadgeInputs{TotalAnswered: 120}))
		if !slices.Contains(got, "volume-25") || !slices.Contains(got, "volume-100") || slices.Contains(got, "volume-500") {
			t.Fatalf("%v", got)
		}
	})

	t.Run("mastery bar is the planner's number", func(t *testing.T) {
		badges := EarnedBadges(BadgeInputs{TopicsMastered: 1})
		if MasteryBar != 85 || len(badges) == 0 || !strings.Contains(badges[0].Description, "85") {
			t.Fatalf("%+v", badges)
		}
	})

	t.Run("stable across recomputes", func(t *testing.T) {
		state := BadgeInputs{
			MaxStreak: 30, ActiveDates: run("2026-08-14", 30), TotalAnswered: 300,
			TopicsMastered: 6, AccuracyPercent: ptr(72.0), ReviewsCompleted: 12,
		}
		if !reflect.DeepEqual(EarnedBadges(state), EarnedBadges(state)) {
			t.Fatal("recomputes differ")
		}
	})

	t.Run("most recent first", func(t *testing.T) {
		var dated []string
		for _, b := range EarnedBadges(BadgeInputs{MaxStreak: 30, ActiveDates: run("2026-08-14", 30), TotalAnswered: 200}) {
			if b.EarnedOn != nil {
				dated = append(dated, *b.EarnedOn)
			}
		}
		if len(dated) <= 1 {
			t.Fatalf("dated %v", dated)
		}
		for i := 1; i < len(dated); i++ {
			if dated[i-1] < dated[i] {
				t.Fatalf("not newest first: %v", dated)
			}
		}
	})
}

func TestStreakReachedOn(t *testing.T) {
	// A 30-day run contains the moment the 7-day badge was earned: day seven.
	dates := run("2026-08-14", 30)
	early, late := run("2026-03-10", 8), run("2026-08-14", 20)
	ten := run("2026-08-14", 10)
	shuffled := slices.Concat([]string{ten[5], ten[0], ten[9]}, ten[1:5], ten[6:9])

	for _, c := range []struct {
		name   string
		dates  []string
		length int
		want   *string
	}{
		{"day the target was hit", dates, 7, &dates[6]},
		{"earliest qualifying run", slices.Concat(early, late), 7, &early[6]},
		{"run spanning a gap", slices.Concat(run("2026-07-01", 3), run("2026-07-20", 3)), 5, nil},
		{"run never happened", run("2026-08-14", 3), 30, nil},
		{"no dates", nil, 3, nil},
		{"order does not matter", shuffled, 7, StreakReachedOn(ten, 7)},
	} {
		if got := StreakReachedOn(c.dates, c.length); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, deref(got), deref(c.want))
		}
	}
}

func progressFor(next []BadgeProgress, family BadgeFamily) *BadgeProgress {
	for i := range next {
		if next[i].Family == family {
			return &next[i]
		}
	}
	return nil
}

func TestNextBadges(t *testing.T) {
	// Three days, not thirty: a first rung of a month only rewards people already persisting.
	streak := progressFor(NextBadges(BadgeInputs{}), FamilyStreak)
	if streak == nil || streak.Target != 3 || streak.Current != 0 {
		t.Fatalf("%+v", streak)
	}

	next := NextBadges(BadgeInputs{MaxStreak: 7, TotalAnswered: 30})
	if p := progressFor(next, FamilyStreak); p == nil || p.Target != 30 {
		t.Fatalf("streak %+v", p)
	}
	if p := progressFor(next, FamilyVolume); p == nil || p.Target != 100 {
		t.Fatalf("volume %+v", p)
	}

	// The top of the list is the one actually within reach.
	if next := NextBadges(BadgeInputs{MaxStreak: 29, TotalAnswered: 30}); next[0].Family != FamilyStreak {
		t.Fatalf("first %+v", next[0])
	}

	// A family with nothing left to reach is dropped.
	if p := progressFor(NextBadges(BadgeInputs{MaxStreak: 500, ActiveDates: run("2026-08-14", 200)}), FamilyStreak); p != nil {
		t.Fatalf("streak %+v", p)
	}

	for _, state := range []BadgeInputs{
		{TotalAnswered: 99},
		{MaxStreak: 2},
		{TotalAnswered: 40, AccuracyPercent: ptr(69.0)},
	} {
		for _, p := range NextBadges(state) {
			if p.Current >= p.Target {
				t.Errorf("%+v beyond target", p)
			}
		}
	}
}
