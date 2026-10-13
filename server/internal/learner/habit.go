package learner

import (
	"math"

	"github.com/shaunmortal/AI-tutor/server/internal/dates"
)

// Streaks, consistency and answering speed: how somebody studies rather than what they know.
//
// A streak does not need the history walked day by day; the previous streak and the date
// it was last touched are enough:
//
//	last active today        -> unchanged, they already practised today
//	last active yesterday    -> one more
//	anything older, or never -> back to one, today
//
// That is O(1) on the path that runs after every answer. Consistency is the exception:
// "how many of the days available did you study" needs a count of distinct days, which a
// streak cannot give, so the caller passes it in from the attempts themselves.

// HabitState is the learner's habits before this answer.
type HabitState struct {
	CurrentStreakDays int `json:"currentStreakDays"`
	LongestStreakDays int `json:"longestStreakDays"`
	// YYYY-MM-DD, or nil for a learner who has never answered anything.
	LastActivityDate      *string  `json:"lastActivityDate"`
	AvgSecondsPerQuestion *float64 `json:"avgSecondsPerQuestion"`
	// Answers recorded before this one.
	TotalAttempts int `json:"totalAttempts"`
}

// PracticeSpan is the distinct days a learner has practised on, and the first of them.
type PracticeSpan struct {
	DistinctDays int    `json:"distinctDays"`
	FirstDate    string `json:"firstDate"`
}

// HabitEvent is one more answer.
type HabitEvent struct {
	// YYYY-MM-DD in the learner's own reckoning.
	Today string `json:"today"`
	// How long this answer took, when the client reported it.
	SecondsTaken *float64 `json:"secondsTaken"`
	// Counted from the attempts table, because a streak cannot tell you about the days it
	// was broken. Nil when not worth a query, and consistency is then left unset rather
	// than guessed at.
	Practice *PracticeSpan `json:"practice,omitempty"`
}

// HabitUpdate is the learner's habits after this answer.
type HabitUpdate struct {
	CurrentStreakDays     int      `json:"currentStreakDays"`
	LongestStreakDays     int      `json:"longestStreakDays"`
	LastActivityDate      string   `json:"lastActivityDate"`
	AvgSecondsPerQuestion *float64 `json:"avgSecondsPerQuestion"`
	ConsistencyScore      *float64 `json:"consistencyScore"`
}

// Longest gap that still keeps a streak: yesterday, and nothing further back.
const streakGapDays = 1

// Speed is capped before it is averaged. A learner who leaves the tab and answers an hour
// later did not spend an hour thinking, and one such answer would make the time estimates
// a study plan's "20 minutes" rests on useless. Generous: a hard question can take minutes.
const maxCreditedSeconds = 600.0

// NextHabits is the learner's habits after one more answer. It never lowers
// LongestStreakDays: the database CHECKs current <= longest, so the two move together or
// the write is rejected.
func NextHabits(previous HabitState, event HabitEvent) HabitUpdate {
	current := nextStreak(previous.CurrentStreakDays, previous.LastActivityDate, event.Today)

	u := HabitUpdate{
		CurrentStreakDays:     current,
		LongestStreakDays:     max(previous.LongestStreakDays, current),
		LastActivityDate:      event.Today,
		AvgSecondsPerQuestion: nextAverageSeconds(previous, event.SecondsTaken),
	}
	if event.Practice != nil {
		u.ConsistencyScore = ConsistencyPercent(*event.Practice, event.Today)
	}
	return u
}

func nextStreak(previousStreak int, lastActivityDate *string, today string) int {
	if lastActivityDate == nil || *lastActivityDate == "" {
		return 1
	}

	gap, ok := dates.DaysBetween(*lastActivityDate, today)

	// A future or unparseable date means the clock is wrong, not the learner. Leaving the
	// streak alone is the conservative answer; resetting it would punish them for it.
	if !ok || gap <= 0 {
		return max(previousStreak, 1)
	}
	if gap <= streakGapDays {
		return previousStreak + 1
	}
	return 1
}

// nextAverageSeconds keeps a running mean so the history need not be re-read. With no
// average yet (an older profile, or earlier answers without timings) this answer starts
// one rather than being averaged into nothing.
func nextAverageSeconds(previous HabitState, secondsTaken *float64) *float64 {
	if secondsTaken == nil || math.IsNaN(*secondsTaken) || math.IsInf(*secondsTaken, 0) || *secondsTaken < 0 {
		return previous.AvgSecondsPerQuestion
	}

	credited := min(*secondsTaken, maxCreditedSeconds)

	if previous.AvgSecondsPerQuestion == nil || previous.TotalAttempts <= 0 {
		return ptr(round2(credited))
	}

	total := *previous.AvgSecondsPerQuestion*float64(previous.TotalAttempts) + credited
	return ptr(round2(total / float64(previous.TotalAttempts+1)))
}

// ConsistencyPercent is days practised as a percentage of days available, counted from
// the learner's first answer rather than sign-up: somebody who registered in January and
// started in June is new, not 5% consistent. The span includes today, so a first-day
// learner scores 100 rather than dividing by zero. Nil when there is nothing to measure.
func ConsistencyPercent(practice PracticeSpan, today string) *float64 {
	days, ok := dates.DaysBetween(practice.FirstDate, today)
	span := days + 1
	if !ok || span <= 0 || practice.DistinctDays <= 0 {
		return nil
	}

	// Clamped rather than trusted: a client/server timezone disagreement could give 105%.
	return ptr(round2(min(100, float64(practice.DistinctDays)/float64(span)*100)))
}
