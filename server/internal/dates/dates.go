// Package dates is the learner's calendar day, and arithmetic on YYYY-MM-DD strings.
//
// Everything that counts days — a streak, whether a revision is due, whether today's session
// was missed — has to agree with the day the learner thinks they are in. The UTC date does not:
// for the timezone this app is built for it is a different day for five and a half hours out of
// every twenty-four. At 00:56 IST on 14 August the server once believed it was the 13th, so a
// learner studying at 01:00 and 10:00 the same morning crossed a UTC midnight, counted as two
// days, inflated their streak and double-stepped the revision schedule. Nothing errored.
package dates

import (
	"fmt"
	"strconv"
	"time"
	_ "time/tzdata" // the TS used the runtime's own tz database; embed one so a bare container agrees
)

// DefaultTimezone is the fallback when a profile has no (or a junk) timezone. The column is
// `not null default 'Asia/Kolkata'`, so this should be unreachable — but UTC would be the one
// answer guaranteed to be wrong for these learners.
const DefaultTimezone = "Asia/Kolkata"

const layout = "2006-01-02"

// TodayIn is today's date as the learner in timezone sees it, YYYY-MM-DD. An empty or
// unrecognised timezone falls back to DefaultTimezone: losing a submitted answer over a
// formatting call is worse than a fallback date.
func TodayIn(timezone string, now time.Time) string {
	loc, err := time.LoadLocation(timezone)
	// "" and "Local" are valid to Go but mean UTC / the server's zone; Intl rejects both.
	if err != nil || timezone == "" || timezone == "Local" {
		loc, _ = time.LoadLocation(DefaultTimezone)
	}
	return now.In(loc).Format(layout)
}

// ParseDay reads YYYY-MM-DD as UTC midnight. Like JS Date.parse it accepts a day past the end
// of the month (2026-02-30 is 2 March) but rejects month 13 or a non-padded field.
func ParseDay(date string) (time.Time, bool) {
	if len(date) != 10 || date[4] != '-' || date[7] != '-' {
		return time.Time{}, false
	}
	for i, c := range date {
		if i != 4 && i != 7 && (c < '0' || c > '9') {
			return time.Time{}, false
		}
	}
	y, _ := strconv.Atoi(date[:4])
	m, _ := strconv.Atoi(date[5:7])
	d, _ := strconv.Atoi(date[8:])
	if m < 1 || m > 12 || d < 1 || d > 31 {
		return time.Time{}, false
	}
	return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC), true
}

// AddDays shifts a YYYY-MM-DD date by whole days, in UTC so a timezone cannot shift the answer.
// It panics on a malformed date, as the TS threw a RangeError: callers only pass dates from
// TodayIn or a `date` column.
func AddDays(date string, days int) string {
	t, ok := ParseDay(date)
	if !ok {
		panic(fmt.Sprintf("dates.AddDays: invalid date %q", date))
	}
	return t.AddDate(0, 0, days).Format(layout)
}

// DaysBetween is the whole days from one YYYY-MM-DD date to another, in UTC. ok is false when
// either date is malformed (the TS returned NaN).
func DaysBetween(from, to string) (days int, ok bool) {
	start, ok1 := ParseDay(from)
	end, ok2 := ParseDay(to)
	if !ok1 || !ok2 {
		return 0, false
	}
	return int((end.Unix() - start.Unix()) / 86400), true
}
