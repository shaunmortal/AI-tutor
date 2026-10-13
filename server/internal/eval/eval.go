// Package eval is the pure scoring for cmd/evalgrounding, kept apart so it is unit-tested
// without a database.
package eval

import "math"

// Expected is a location the answer to a test question is known to be at.
type Expected struct {
	Source   string `json:"source"`
	Page     *int   `json:"page,omitempty"`
	Slide    *int   `json:"slide,omitempty"`
	StartSec *int   `json:"startSec,omitempty"`
	EndSec   *int   `json:"endSec,omitempty"`
}

// Located is a retrieved or cited excerpt with its raw location. Page holds the slide number
// for slides.
type Located struct {
	SourceTitle string
	Page        *int
	StartSec    *int
	EndSec      *int
}

// Matches says whether an excerpt sits at one of the expected locations.
func Matches(c Located, expected []Expected) bool {
	for _, place := range expected {
		if place.Source != c.SourceTitle {
			continue
		}
		switch {
		case place.Page != nil:
			if c.Page != nil && *c.Page == *place.Page {
				return true
			}
		case place.Slide != nil:
			if c.Page != nil && *c.Page == *place.Slide {
				return true
			}
		case place.StartSec != nil && c.StartSec != nil:
			end := *c.StartSec
			if c.EndSec != nil {
				end = *c.EndSec
			}
			placeEnd := *place.StartSec
			if place.EndSec != nil {
				placeEnd = *place.EndSec
			}
			// Transcript windows are cut differently from the test set's ranges, so any overlap counts.
			if *c.StartSec <= placeEnd && end >= *place.StartSec {
				return true
			}
		}
	}
	return false
}

// Row is one line of eval/results/grounding.jsonl; eval/ragas_eval.py reads these field names.
type Row struct {
	ID                string     `json:"id"`
	Type              string     `json:"type"` // in_material, off_material or figure
	Question          string     `json:"question"`
	Reference         string     `json:"reference"`
	Answer            string     `json:"answer"`
	Grounding         string     `json:"grounding"`
	Contexts          []string   `json:"contexts"`
	Retrieved         []string   `json:"retrieved"`
	Cited             []string   `json:"cited"`
	Expected          []Expected `json:"expected"`
	RetrievalHit      *bool      `json:"retrievalHit"`
	CitationHit       *bool      `json:"citationHit"`
	CitationPrecision *float64   `json:"citationPrecision"`
	Refused           bool       `json:"refused"`
}

// Summary is grounding-summary.json. Rates are percentages to one decimal, nil with no questions.
type Summary struct {
	Questions             int      `json:"questions"`
	InMaterial            int      `json:"inMaterial"`
	OffMaterial           int      `json:"offMaterial"`
	RetrievalHitRatePct   *float64 `json:"retrievalHitRatePct"`
	CitationAccuracyPct   *float64 `json:"citationAccuracyPct"`
	CitationPrecisionPct  *float64 `json:"citationPrecisionPct"`
	FalseRefusalRatePct   *float64 `json:"falseRefusalRatePct"`
	CorrectRefusalRatePct *float64 `json:"correctRefusalRatePct"`
}

// pct1 is Math.round(x * 1000) / 10, as the TypeScript script rounded.
func pct1(x float64) *float64 {
	v := math.Floor(float64(x*1000)+0.5) / 10
	return &v
}

func isTrue(b *bool) bool { return b != nil && *b }

// Summarise computes the rates over the right subsets: hits and false refusals over the
// in-material questions (figures included), correct refusals over the off-material ones.
func Summarise(rows []Row) Summary {
	var in, off []Row
	for _, r := range rows {
		if r.Type == "off_material" {
			off = append(off, r)
		} else {
			in = append(in, r)
		}
	}
	rate := func(xs []Row, test func(Row) bool) *float64 {
		if len(xs) == 0 {
			return nil
		}
		n := 0
		for _, r := range xs {
			if test(r) {
				n++
			}
		}
		return pct1(float64(n) / float64(len(xs)))
	}
	s := Summary{
		Questions:             len(rows),
		InMaterial:            len(in),
		OffMaterial:           len(off),
		RetrievalHitRatePct:   rate(in, func(r Row) bool { return isTrue(r.RetrievalHit) }),
		CitationAccuracyPct:   rate(in, func(r Row) bool { return isTrue(r.CitationHit) }),
		FalseRefusalRatePct:   rate(in, func(r Row) bool { return r.Refused }),
		CorrectRefusalRatePct: rate(off, func(r Row) bool { return r.Refused }),
	}
	sum, n := 0.0, 0
	for _, r := range in {
		if r.CitationPrecision != nil {
			sum += *r.CitationPrecision
			n++
		}
	}
	if n > 0 {
		s.CitationPrecisionPct = pct1(sum / float64(n))
	}
	return s
}
