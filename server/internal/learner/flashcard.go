package learner

// FlashcardReview is the schedule after a learner marks a card on a topic "known" or "again", or
// nil when the schedule should not move. A card is one piece of evidence, so it counts as a
// one-question review (thin evidence: a pass steps the interval gently, a miss collapses it).
// As with practice, a topic moves once a day: a second "known" the same day changes nothing, and
// only an "again" can still collapse an interval that has not already collapsed.
func FlashcardReview(state *ReviewState, known bool, today string, daysUntilExam *int) *ScheduleUpdate {
	if state != nil && state.LastReviewedOn != nil && *state.LastReviewedOn == today && (known || state.IntervalDays <= FirstIntervalDays) {
		return nil
	}
	accuracy := 0.0
	if known {
		accuracy = 100
	}
	u := NextReview(state, ReviewOutcome{ReviewedOn: today, AccuracyPercent: accuracy, QuestionsAnswered: 1, DaysUntilExam: daysUntilExam})
	return &u
}
