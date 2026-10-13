// Package adaptive decides what a learner should do next: which topic, what kind of
// session, at what difficulty, and which questions to serve for it.
//
// Deterministic. No AI is involved in the decision -- Claude is only ever asked to explain
// a decision that has already been made here, so the reasoning stays inspectable and the
// same inputs always give the same answer.
package adaptive

import (
	"fmt"
	"math"
)

// Difficulty is a question difficulty level.
type Difficulty string

// The difficulty ladder, easiest first.
const (
	Easy   Difficulty = "easy"
	Medium Difficulty = "medium"
	Hard   Difficulty = "hard"
)

// Adjustment says whether recent form overrode what mastery alone would have picked.
type Adjustment string

// Adjustment values.
const (
	AdjustNone        Adjustment = "none"
	AdjustSteppedBack Adjustment = "stepped_back"
	AdjustSteppedUp   Adjustment = "stepped_up"
)

// The goal is to keep the learner just past comfortable. Questions that are too easy teach
// nothing and inflate the mastery score; questions that are too hard produce a string of
// wrong answers. So difficulty follows mastery, with one override: if recent answers are
// going badly, step back regardless of the score.
const (
	// Below this mastery, stay on easy questions.
	easyBelow = 40
	// At or above this mastery, move to hard questions.
	hardAtOrAbove = 70
	// If recent accuracy has fallen below this, drop one level even when mastery looks
	// healthy. A score built weeks ago should not keep serving hard questions to somebody
	// who is currently struggling.
	stepBackBelowRecentAccuracy = 0.4
	// Recent accuracy this high earns a step up, once there is enough evidence for it.
	stepUpAboveRecentAccuracy = 0.85
	// Fewer recent answers than this and we do not read anything into them.
	minRecentAttemptsToAdjust = 4
)

var ladder = []Difficulty{Easy, Medium, Hard}

func step(current Difficulty, direction int) Difficulty {
	index := -1
	for i, d := range ladder {
		if d == current {
			index = i
		}
	}
	return ladder[min(len(ladder)-1, max(0, index+direction))]
}

// DifficultySignals is what is known about the learner on one topic.
type DifficultySignals struct {
	MasteryScore float64 `json:"masteryScore"`
	// 0 to 1, or nil when the topic has not been attempted recently.
	RecentAccuracy *float64 `json:"recentAccuracy"`
	RecentAttempts int      `json:"recentAttempts"`
}

// DifficultyChoice is the difficulty to serve and why.
type DifficultyChoice struct {
	Difficulty Difficulty `json:"difficulty"`
	Adjustment Adjustment `json:"adjustment"`
	// Recent accuracy as a percentage, when it was the deciding factor.
	RecentAccuracyPercent *int `json:"recentAccuracyPercent"`
	// Why, in words. Nil when mastery alone decided and there is nothing to add.
	Reason *string `json:"reason"`
}

// ChooseDifficulty picks the difficulty to serve next for one topic.
func ChooseDifficulty(s DifficultySignals) DifficultyChoice {
	// Start from where mastery says the learner is.
	choice := DifficultyChoice{Difficulty: Medium, Adjustment: AdjustNone}
	if s.MasteryScore < easyBelow {
		choice.Difficulty = Easy
	} else if s.MasteryScore >= hardAtOrAbove {
		choice.Difficulty = Hard
	}

	// Then let recent form override it, but only with enough answers to mean something.
	if s.RecentAccuracy == nil || s.RecentAttempts < minRecentAttemptsToAdjust {
		return choice
	}
	recent := *s.RecentAccuracy
	percent := int(jsRound(recent * 100))

	var reason string
	switch {
	case recent < stepBackBelowRecentAccuracy:
		choice.Difficulty = step(choice.Difficulty, -1)
		choice.Adjustment = AdjustSteppedBack
		reason = fmt.Sprintf("recent accuracy is only %d%%, so stepping back to %s", percent, choice.Difficulty)
	case recent > stepUpAboveRecentAccuracy:
		choice.Difficulty = step(choice.Difficulty, 1)
		choice.Adjustment = AdjustSteppedUp
		reason = fmt.Sprintf("recent accuracy is %d%%, so stepping up to %s", percent, choice.Difficulty)
	default:
		return choice
	}
	choice.RecentAccuracyPercent = &percent
	choice.Reason = &reason
	return choice
}

// jsRound matches JavaScript's Math.round, which rounds halves towards +Inf (Go's
// math.Round rounds them away from zero).
func jsRound(x float64) float64 {
	f := math.Floor(x)
	if x-f >= 0.5 {
		return f + 1
	}
	return f
}
