// Package learner holds the pure engines behind the learner model: mastery, spaced
// repetition, the activity calendar, badges and study habits. No database, no AI, no
// clock: everything they look at is passed in, so each number on the dashboard can be
// tested against hand-worked examples.
//
// Dates are YYYY-MM-DD strings in the learner's own reckoning, and all day arithmetic is
// done at UTC midnight so a server timezone can never shift a date already settled.
package learner

import "math"

func clamp(value, low, high float64) float64 {
	return min(high, max(low, value))
}

// jsRound matches JavaScript's Math.round: halves go towards +Inf, whereas math.Round
// goes away from zero (they differ only on negative halves).
func jsRound(x float64) float64 {
	r := math.Round(x)
	if r-x == -0.5 {
		r++
	}
	return r
}

func round2(value float64) float64 {
	return jsRound(value*100) / 100
}

func ptr[T any](v T) *T { return &v }
