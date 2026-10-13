package dates

import (
	"regexp"
	"testing"
	"time"
)

// 01:00 on 14 August in India, which is still 13 August in UTC — the morning of the real bug.
var earlyMorningIST = mustTime("2026-08-13T19:30:00.000Z")

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestTodayIn(t *testing.T) {
	cases := []struct {
		name, zone, now, want string
	}{
		// The learner's own date, not the server's.
		{"learner's date", "Asia/Kolkata", "2026-08-13T19:30:00.000Z", "2026-08-14"},
		// One Indian day stays one day across UTC midnight: 01:00 and 10:00 IST.
		{"01:00 IST", "Asia/Kolkata", "2026-08-13T19:30:00.000Z", "2026-08-14"},
		{"10:00 IST", "Asia/Kolkata", "2026-08-14T04:30:00.000Z", "2026-08-14"},
		// Rolls over at the learner's midnight: 23:59 IST and one minute later.
		{"23:59 IST", "Asia/Kolkata", "2026-08-14T18:29:00.000Z", "2026-08-14"},
		{"00:01 IST", "Asia/Kolkata", "2026-08-14T18:31:00.000Z", "2026-08-15"},
		// Either side of the date line: proves the zone database does the work, not a hardcoded offset.
		{"Kiritimati", "Pacific/Kiritimati", "2026-08-13T19:30:00.000Z", "2026-08-14"},
		{"Midway", "Pacific/Midway", "2026-08-13T19:30:00.000Z", "2026-08-13"},
		// Falls back rather than failing on a missing or nonsense timezone (TS: null, undefined).
		{"missing", "", "2026-08-13T19:30:00.000Z", "2026-08-14"},
		{"nonsense", "Mars/Olympus_Mons", "2026-08-13T19:30:00.000Z", "2026-08-14"},
		{"Local is not a zone", "Local", "2026-08-13T19:30:00.000Z", "2026-08-14"},
	}
	for _, c := range cases {
		if got := TodayIn(c.zone, mustTime(c.now)); got != c.want {
			t.Errorf("%s: TodayIn(%q, %s) = %s, want %s", c.name, c.zone, c.now, got, c.want)
		}
	}

	// The bug, stated as a fact: UTC disagrees, and following UTC is what was wrong.
	if got := earlyMorningIST.UTC().Format(layout); got != "2026-08-13" {
		t.Errorf("UTC date = %s, want 2026-08-13", got)
	}
}

func TestTodayInFormat(t *testing.T) {
	// Every caller writes this into a `date` column or compares it as a string.
	re := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	for _, zone := range []string{"Asia/Kolkata", "UTC", "America/New_York", "Pacific/Kiritimati"} {
		if got := TodayIn(zone, earlyMorningIST); !re.MatchString(got) {
			t.Errorf("TodayIn(%q) = %q, not YYYY-MM-DD", zone, got)
		}
	}
}

func TestAddDays(t *testing.T) {
	cases := []struct {
		date string
		days int
		want string
	}{
		{"2026-08-30", 3, "2026-09-02"},
		{"2026-12-30", 3, "2027-01-02"},
		{"2026-08-14", 0, "2026-08-14"},
		{"2026-08-14", -14, "2026-07-31"},
		{"2026-02-30", 0, "2026-03-02"}, // JS Date.parse rolls an overflowing day forward
	}
	for _, c := range cases {
		if got := AddDays(c.date, c.days); got != c.want {
			t.Errorf("AddDays(%s, %d) = %s, want %s", c.date, c.days, got, c.want)
		}
	}
}

func TestDaysBetween(t *testing.T) {
	cases := []struct {
		from, to string
		want     int
		ok       bool
	}{
		{"2026-08-14", "2026-08-14", 0, true},
		{"2026-08-10", "2026-08-14", 4, true},
		{"2026-08-14", "2026-08-10", -4, true},
		{"2026-12-30", "2027-01-02", 3, true},
		{"2026-13-01", "2026-08-14", 0, false},
		{"2026-08-00", "2026-08-14", 0, false},
		{"2026-8-1", "2026-08-14", 0, false},
		{"", "2026-08-14", 0, false},
	}
	for _, c := range cases {
		got, ok := DaysBetween(c.from, c.to)
		if got != c.want || ok != c.ok {
			t.Errorf("DaysBetween(%q, %q) = %d, %v; want %d, %v", c.from, c.to, got, ok, c.want, c.ok)
		}
	}
}
