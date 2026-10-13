package learner

// SelfRatedPrior is the mastery assumed for a topic the learner has never been measured on,
// from how confident they said they were in its area at onboarding (1 = not at all, 5 = very).
//
// 3 is the neutral prior the planner already assumes for an unknown topic (35), so "average"
// changes nothing. The top is 65: below "solid" (75) and the planner's target (85), because a
// self-rating is a hint, not evidence — it can make a confident area a little less urgent, never
// mark it done. The first real answer replaces it entirely: mastery is computed from attempts only.
func SelfRatedPrior(confidence int) float64 {
	return [...]float64{15, 25, 35, 50, 65}[min(5, max(1, confidence))-1]
}
