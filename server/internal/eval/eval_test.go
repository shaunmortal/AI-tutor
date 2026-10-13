package eval

import "testing"

func p[T any](v T) *T { return &v }

func TestMatchesPageExactlyAndVideoByOverlap(t *testing.T) {
	page := func(src string, n int) Located { return Located{SourceTitle: src, Page: p(n)} }
	video := func(src string, start, end int) Located {
		return Located{SourceTitle: src, StartSec: p(start), EndSec: p(end)}
	}
	l1p3 := []Expected{{Source: "Lecture 1 notes", Page: p(3)}}
	l2v := []Expected{{Source: "Lecture 2 video", StartSec: p(640), EndSec: p(700)}}

	cases := []struct {
		name string
		got  bool
		want bool
	}{
		{"same page", Matches(page("Lecture 1 notes", 3), l1p3), true},
		{"other page", Matches(page("Lecture 1 notes", 4), l1p3), false},
		{"overlapping range", Matches(video("Lecture 2 video", 600, 660), l2v), true},
		{"disjoint range", Matches(video("Lecture 2 video", 100, 160), l2v), false},
		{"other source", Matches(page("Lecture 2 notes", 3), l1p3), false},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestSummariseRatesOverTheRightSubsets(t *testing.T) {
	s := Summarise([]Row{
		{Type: "in_material", RetrievalHit: p(true), CitationHit: p(true), CitationPrecision: p(1.0)},
		{Type: "in_material", RetrievalHit: p(true), CitationHit: p(false), CitationPrecision: p(0.0)},
		{Type: "off_material", Refused: true},
	})
	for name, c := range map[string]struct {
		got  *float64
		want float64
	}{
		"retrievalHitRatePct":   {s.RetrievalHitRatePct, 100},
		"citationAccuracyPct":   {s.CitationAccuracyPct, 50},
		"citationPrecisionPct":  {s.CitationPrecisionPct, 50},
		"falseRefusalRatePct":   {s.FalseRefusalRatePct, 0},
		"correctRefusalRatePct": {s.CorrectRefusalRatePct, 100},
	} {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s: got %v, want %v", name, c.got, c.want)
		}
	}
}
