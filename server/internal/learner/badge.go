package learner

import (
	"github.com/shaunmortal/AI-tutor/server/internal/dates"

	"fmt"
	"sort"
	"time"
)

// Badges are the milestones a learner has actually passed, derived from the record rather
// than stored. With no badges table and no "award" write a badge cannot be wrong: storage
// would be a second source of truth that drifts (a failed write loses one, a replayed one
// duplicates it, a changed threshold makes every row stale). Given up: a permanent award
// date if the criteria change, though for streaks the date is recoverable anyway.

// BadgeFamily groups badges into ladders: streak, volume, mastery, accuracy, revision.
type BadgeFamily string

// The badge families, in the order progress is reported.
const (
	FamilyStreak   BadgeFamily = "streak"
	FamilyVolume   BadgeFamily = "volume"
	FamilyMastery  BadgeFamily = "mastery"
	FamilyAccuracy BadgeFamily = "accuracy"
	FamilyRevision BadgeFamily = "revision"
)

// Badge is one earned milestone.
type Badge struct {
	// Stable across recomputes, so the client can key on it.
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Family BadgeFamily `json:"family"`
	// What it took, in words, for a learner reading their own shelf.
	Description string `json:"description"`
	// YYYY-MM-DD when the criteria were met, when that is recoverable.
	EarnedOn *string `json:"earnedOn"`
}

// BadgeProgress is the next badge in a family and how far along the learner is.
type BadgeProgress struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Family  BadgeFamily `json:"family"`
	Current int         `json:"current"`
	Target  int         `json:"target"`
	// What to show under the bar.
	Description string `json:"description"`
}

// BadgeInputs is everything the badges are judged on.
type BadgeInputs struct {
	// Active days, oldest first. Used for streak dates.
	ActiveDates   []string `json:"activeDates"`
	MaxStreak     int      `json:"maxStreak"`
	TotalAnswered int      `json:"totalAnswered"`
	// Topics at or above MasteryBar.
	TopicsMastered int `json:"topicsMastered"`
	// Overall accuracy 0-100, or nil with too little evidence.
	AccuracyPercent *float64 `json:"accuracyPercent"`
	// Reviews completed through the spaced-repetition schedule.
	ReviewsCompleted int `json:"reviewsCompleted"`
}

// MasteryBar is the mastery a topic counts as learned at: the same 85 the planner stops
// allocating fresh time at, so no screen calls a topic mastered while another still
// schedules work on it.
const MasteryBar = 85

// Answers needed before an accuracy badge means anything.
const minAnswersForAccuracy = 30

type tier struct {
	family      BadgeFamily
	target      int
	id, name    string
	description string
}

func streakTier(n int, id, name string) tier {
	return tier{FamilyStreak, n, id, name, fmt.Sprintf("Studied %d days in a row.", n)}
}

func volumeTier(n int, id, name string) tier {
	return tier{FamilyVolume, n, id, name, fmt.Sprintf("Answered %d questions.", n)}
}

func masteryTier(n int, id, name string) tier {
	return tier{FamilyMastery, n, id, name, fmt.Sprintf("Took %d topics past %d mastery.", n, MasteryBar)}
}

func accuracyTier(n int, id, name string) tier {
	return tier{FamilyAccuracy, n, id, name, fmt.Sprintf("%d%% accuracy over %d+ questions.", n, minAnswersForAccuracy)}
}

func revisionTier(n int, id, name string) tier {
	return tier{FamilyRevision, n, id, name, fmt.Sprintf("Completed %d scheduled revisions.", n)}
}

// tiers is the whole ladder, deliberately shallow at the bottom: the first streak badge is
// three days, not thirty, so it is reachable by somebody who has not yet decided whether
// they are the sort of person who keeps a streak.
var tiers = []tier{
	streakTier(3, "streak-3", "Getting Started"),
	streakTier(7, "streak-7", "One Week"),
	streakTier(30, "streak-30", "One Month"),
	streakTier(50, "streak-50", "50 Days"),
	streakTier(100, "streak-100", "Century"),

	volumeTier(25, "volume-25", "First 25"),
	volumeTier(100, "volume-100", "Hundred Up"),
	volumeTier(500, "volume-500", "Five Hundred"),
	volumeTier(1000, "volume-1000", "Thousand Club"),

	{FamilyMastery, 1, "mastery-1", "First Topic Mastered", fmt.Sprintf("Took a topic past %d mastery.", MasteryBar)},
	masteryTier(5, "mastery-5", "Five Mastered"),
	masteryTier(15, "mastery-15", "Fifteen Mastered"),

	accuracyTier(70, "accuracy-70", "Steady Hand"),
	accuracyTier(85, "accuracy-85", "Sharp"),

	revisionTier(10, "revision-10", "Comes Back"),
	revisionTier(50, "revision-50", "Never Forgets"),
}

func valueFor(family BadgeFamily, in BadgeInputs) float64 {
	switch family {
	case FamilyStreak:
		return float64(in.MaxStreak)
	case FamilyVolume:
		return float64(in.TotalAnswered)
	case FamilyMastery:
		return float64(in.TopicsMastered)
	case FamilyAccuracy:
		// Gated on evidence: 100% from four questions is a small sample, not accuracy.
		if in.TotalAnswered >= minAnswersForAccuracy && in.AccuracyPercent != nil {
			return *in.AccuracyPercent
		}
		return 0
	case FamilyRevision:
		return float64(in.ReviewsCompleted)
	}
	return 0
}

// StreakReachedOn is the date a streak of `length` was first reached, or nil if it never
// was. Dates may arrive in any order. It returns the day the target was hit in the
// earliest qualifying run, since that is when the badge was earned.
func StreakReachedOn(activeDates []string, length int) *string {
	if length <= 0 || len(activeDates) < length {
		return nil
	}

	sorted := append([]string(nil), activeDates...)
	sort.Strings(sorted)

	run := 0
	for i, date := range sorted {
		consecutive := false
		if i > 0 {
			prev, ok1 := dates.ParseDay(sorted[i-1])
			cur, ok2 := dates.ParseDay(date)
			consecutive = ok1 && ok2 && cur.Sub(prev) == 24*time.Hour
		}
		if consecutive {
			run++
		} else {
			run = 1
		}
		if run >= length {
			return &date
		}
	}
	return nil
}

// EarnedBadges is every badge the record supports, newest first.
func EarnedBadges(in BadgeInputs) []Badge {
	earned := []Badge{}

	for _, t := range tiers {
		if valueFor(t.family, in) < float64(t.target) {
			continue
		}
		// Only streaks can say when. Claiming a date for the others would mean inventing one.
		var earnedOn *string
		if t.family == FamilyStreak {
			earnedOn = StreakReachedOn(in.ActiveDates, t.target)
		}
		earned = append(earned, Badge{t.id, t.name, t.family, t.description, earnedOn})
	}

	// Newest first, as a learner reads their own shelf; undated badges sort after dated ones.
	sort.SliceStable(earned, func(i, j int) bool {
		return deref(earned[i].EarnedOn) > deref(earned[j].EarnedOn)
	})
	return earned
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// NextBadges is the next badge in each family with progress, closest to completion first.
// This is the part that motivates: "two more days" is a reason to open the app tomorrow.
// Families already complete are left out.
func NextBadges(in BadgeInputs) []BadgeProgress {
	upcoming := []BadgeProgress{}

	for _, family := range []BadgeFamily{FamilyStreak, FamilyVolume, FamilyMastery, FamilyAccuracy, FamilyRevision} {
		current := valueFor(family, in)
		for _, t := range tiers {
			if t.family == family && current < float64(t.target) {
				upcoming = append(upcoming, BadgeProgress{
					t.id, t.name, family, int(jsRound(current)), t.target, t.description,
				})
				break
			}
		}
	}

	// Closest first, so the top of the list is the one actually within reach.
	share := func(p BadgeProgress) float64 { return float64(p.Current) / float64(p.Target) }
	sort.SliceStable(upcoming, func(i, j int) bool { return share(upcoming[i]) > share(upcoming[j]) })
	return upcoming
}
