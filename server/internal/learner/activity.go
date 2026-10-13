package learner

import "github.com/shaunmortal/AI-tutor/server/internal/dates"

// The activity calendar: how much was answered on each of the last 365 days, the same
// shape as GitHub's contribution grid, because a year of effort at a glance shows the
// gaps as well as the work.
//
// The streaks are derived from the day list here rather than read from
// learner_profiles.current_streak_days, which is maintained forward one answer at a time
// and knows nothing about history written before it existed. Deriving them means the
// number on screen always agrees with the squares next to it.
//
// A day is a day in the learner's own reckoning, so callers group timestamps into local
// dates before they arrive here.

// CalendarDays is the window: a year, as LeetCode and GitHub show it.
const CalendarDays = 365

// ActivityDay is one square of the grid.
type ActivityDay struct {
	// YYYY-MM-DD.
	Date  string `json:"date"`
	Count int    `json:"count"`
	// 0 to 4, for the colour ramp. 0 means nothing was answered.
	Level int `json:"level"`
}

// ActivityCalendar is the whole grid plus the numbers read off it.
type ActivityCalendar struct {
	Days []ActivityDay `json:"days"`
	// Questions answered across the window.
	TotalAnswered int `json:"totalAnswered"`
	// Days with at least one answer.
	ActiveDays int `json:"activeDays"`
	// Longest unbroken run of active days in the window.
	MaxStreak int `json:"maxStreak"`
	// Unbroken run ending today, or yesterday if today has no answers yet.
	CurrentStreak int `json:"currentStreak"`
	// The window, so the client does not have to guess what it is looking at.
	From string `json:"from"`
	To   string `json:"to"`
}

// LevelFor maps an answer count to a shade. Thresholds 1, 3, 6, 10 are set for this
// app's scale: a session is a handful of questions, so the top shade has to be reachable
// by somebody having a genuinely good day.
func LevelFor(count int) int {
	switch {
	case count <= 0:
		return 0
	case count < 3:
		return 1
	case count < 6:
		return 2
	case count < 10:
		return 3
	}
	return 4
}

// BuildCalendar builds the CalendarDays-long grid ending today from a count per active
// day. It takes counts rather than timestamps because grouping into local days needs the
// learner's timezone, which the caller already has. Every day appears, including empty
// ones: a client that had to fill gaps itself could get the gaps wrong.
func BuildCalendar(countsByDate map[string]int, today string) ActivityCalendar {
	from := dates.AddDays(today, -(CalendarDays - 1))
	cal := ActivityCalendar{Days: make([]ActivityDay, 0, CalendarDays), From: from, To: today}

	running := 0
	for offset := range CalendarDays {
		date := dates.AddDays(from, offset)
		count := countsByDate[date]
		cal.Days = append(cal.Days, ActivityDay{Date: date, Count: count, Level: LevelFor(count)})
		cal.TotalAnswered += count

		if count > 0 {
			cal.ActiveDays++
			running++
			cal.MaxStreak = max(cal.MaxStreak, running)
		} else {
			running = 0
		}
	}

	cal.CurrentStreak = currentStreakFrom(countsByDate, today)
	return cal
}

// currentStreakFrom is the run ending now. A day with nothing answered yet does not break
// it: somebody opening the app at nine in the morning has not lost their streak, they have
// not had the chance to keep it. So the walk starts at yesterday when today is empty,
// which is what LeetCode and GitHub both do.
func currentStreakFrom(countsByDate map[string]int, today string) int {
	cursor := today
	if countsByDate[today] <= 0 {
		cursor = dates.AddDays(today, -1)
	}

	streak := 0
	// Bounded by the window: an unbroken year is possible and the loop must still end.
	for range CalendarDays + 1 {
		if countsByDate[cursor] <= 0 {
			break
		}
		streak++
		cursor = dates.AddDays(cursor, -1)
	}
	return streak
}
