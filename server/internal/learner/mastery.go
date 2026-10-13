package learner

import "time"

// Mastery turns a learner's answers on one topic into an estimate between 0 and 100.
//
// Deterministic, because this number drives the study plan, the next recommendation and
// the readiness score, and all three have to be explainable. It is an estimate from a
// handful of answers, not a measurement, and two things keep it from overclaiming:
//
//	shrinkage  with little evidence the score is pulled towards a neutral middle, so
//	           three lucky answers do not read as mastery
//	decay      a topic untouched for weeks scores slightly lower, because knowledge fades
//
// Every tunable lives in the constants below.
const (
	// Component weights; must add up to 1. Recent performance leads, because someone who
	// has just revised a topic knows it better than their six-week-old average suggests.
	// History is a counterweight so one good session cannot erase a long weak record.
	weightRecentAccuracy     = 0.4
	weightHistoricalAccuracy = 0.25
	weightDifficultyHandling = 0.25
	weightConsistency        = 0.1

	// How many of the latest attempts count as "recent".
	recentWindow = 10

	// Harder questions say more: easy 1, medium 2, hard 3.
	weightEasy   = 1.0
	weightMedium = 2.0
	weightHard   = 3.0

	// How much of the difficulty component an easy-only record can earn. Weighting
	// accuracy by difficulty is not enough on its own: all-correct easy answers have
	// perfect weighted accuracy, which would read the same as clearing hard ones.
	difficultyLevelFloor = 0.6

	// A correct run of this length counts as full marks for consistency.
	streakForFullConsistency = 5

	// Shrinkage: with n attempts the score is n/(n+evidenceHalfWeight) of the way from the
	// neutral prior to the raw score. Halfway at 5 attempts, 80 percent at 20.
	evidenceHalfWeight = 5.0

	// Where an unproven topic sits: neither known nor unknown.
	neutralPrior = 0.35

	// After this many idle days the full decay applies.
	decayAfterDays = 30.0

	// The most decay can ever take away. Retention risk handles the rest separately.
	maxDecay = 0.15
)

// DifficultyBucket counts the attempts at one difficulty level.
type DifficultyBucket struct {
	Attempts int `json:"attempts"`
	Correct  int `json:"correct"`
}

// TopicEvidence is everything the mastery formula is allowed to look at.
type TopicEvidence struct {
	TotalAttempts   int              `json:"totalAttempts"`
	CorrectAttempts int              `json:"correctAttempts"`
	RecentAttempts  int              `json:"recentAttempts"`
	RecentCorrect   int              `json:"recentCorrect"`
	Easy            DifficultyBucket `json:"easy"`
	Medium          DifficultyBucket `json:"medium"`
	Hard            DifficultyBucket `json:"hard"`
	CorrectStreak   int              `json:"correctStreak"`
	// Nil when the topic has never been attempted.
	DaysSinceLastAttempt *float64 `json:"daysSinceLastAttempt"`
}

// MasteryBreakdown is the parts a mastery score was built from, so a screen can explain it.
type MasteryBreakdown struct {
	RecentAccuracy     *float64 `json:"recentAccuracy"`
	HistoricalAccuracy *float64 `json:"historicalAccuracy"`
	DifficultyHandling *float64 `json:"difficultyHandling"`
	Consistency        float64  `json:"consistency"`
	RawScore           float64  `json:"rawScore"`
	Confidence         float64  `json:"confidence"`
	DecayFactor        float64  `json:"decayFactor"`
}

// MasteryResult is the score plus its breakdown.
type MasteryResult struct {
	Score     float64          `json:"score"`
	Breakdown MasteryBreakdown `json:"breakdown"`
}

func ratio(correct, attempts int) *float64 {
	if attempts > 0 {
		return ptr(float64(correct) / float64(attempts))
	}
	return nil
}

func round2Ptr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	return ptr(round2(*v))
}

// difficultyHandling is accuracy weighted so harder questions count for more, times a
// factor for the level actually attempted. Buckets with no attempts are left out rather
// than counted as zeros: never trying a hard question is not failing every hard question.
// The level factor keeps an easy-only record from full marks without punishing it as
// though the hard questions were failed.
func difficultyHandling(e TopicEvidence) *float64 {
	buckets := []struct {
		bucket DifficultyBucket
		weight float64
	}{{e.Easy, weightEasy}, {e.Medium, weightMedium}, {e.Hard, weightHard}}

	var weightedSum, totalWeight, levelSum float64
	attemptCount, used := 0, 0
	for _, b := range buckets {
		if b.bucket.Attempts <= 0 {
			continue
		}
		used++
		weightedSum += b.weight * (float64(b.bucket.Correct) / float64(b.bucket.Attempts))
		totalWeight += b.weight
		attemptCount += b.bucket.Attempts
		levelSum += b.weight * float64(b.bucket.Attempts)
	}
	if used == 0 {
		return nil
	}

	weightedAccuracy := weightedSum / totalWeight
	// Average difficulty attempted as a fraction of the hardest: easy only 1/3, hard only 1.
	attemptedLevel := levelSum / (float64(attemptCount) * weightHard)
	levelFactor := difficultyLevelFloor + (1-difficultyLevelFloor)*attemptedLevel

	return ptr(weightedAccuracy * levelFactor)
}

// ComputeMastery scores one topic from its evidence.
func ComputeMastery(e TopicEvidence) MasteryResult {
	recent := ratio(e.RecentCorrect, e.RecentAttempts)
	historical := ratio(e.CorrectAttempts, e.TotalAttempts)
	difficulty := difficultyHandling(e)
	consistency := clamp(float64(e.CorrectStreak)/streakForFullConsistency, 0, 1)

	// Components with no evidence are dropped and the remaining weights rescaled, so a
	// missing component cannot silently drag the score towards zero.
	components := []struct {
		value  *float64
		weight float64
	}{
		{recent, weightRecentAccuracy},
		{historical, weightHistoricalAccuracy},
		{difficulty, weightDifficultyHandling},
		{&consistency, weightConsistency},
	}
	var usedWeight, weightedSum float64
	for _, c := range components {
		if c.value != nil {
			usedWeight += c.weight
			weightedSum += c.weight * *c.value
		}
	}
	rawScore := neutralPrior
	if usedWeight > 0 {
		rawScore = weightedSum / usedWeight
	}

	// Shrinkage towards the neutral prior when the evidence is thin.
	confidence := float64(e.TotalAttempts) / (float64(e.TotalAttempts) + evidenceHalfWeight)
	shrunk := rawScore*confidence + neutralPrior*(1-confidence)

	// Mild decay for a topic left alone, capped because a well-learned topic does not
	// become unknown in a month.
	idleDays := 0.0
	if e.DaysSinceLastAttempt != nil {
		idleDays = *e.DaysSinceLastAttempt
	}
	decayFactor := 1 - clamp(idleDays/decayAfterDays, 0, 1)*maxDecay

	return MasteryResult{
		Score: round2(clamp(shrunk*decayFactor*100, 0, 100)),
		Breakdown: MasteryBreakdown{
			RecentAccuracy:     round2Ptr(recent),
			HistoricalAccuracy: round2Ptr(historical),
			DifficultyHandling: round2Ptr(difficulty),
			Consistency:        round2(consistency),
			RawScore:           round2(rawScore),
			Confidence:         round2(confidence),
			DecayFactor:        round2(decayFactor),
		},
	}
}

// AttemptRecord is one answered question, as the recompute reads it out of the attempt log.
type AttemptRecord struct {
	IsCorrect bool `json:"isCorrect"`
	// "easy", "medium" or "hard".
	Difficulty  string    `json:"difficulty"`
	AttemptedAt time.Time `json:"attemptedAt"`
}

// SummariseAttempts folds an attempt log, newest first, into the evidence the formula needs.
//
// Mastery is recomputed from the log rather than nudged in place. It costs one query per
// answer and removes a whole class of bug: a cache that drifts from the history it claims
// to summarise. The log stays the single source of truth.
func SummariseAttempts(attempts []AttemptRecord, now time.Time) TopicEvidence {
	e := TopicEvidence{
		TotalAttempts:  len(attempts),
		RecentAttempts: min(len(attempts), recentWindow),
	}
	if len(attempts) == 0 {
		return e
	}

	for i, a := range attempts {
		var bucket *DifficultyBucket
		switch a.Difficulty {
		case "easy":
			bucket = &e.Easy
		case "medium":
			bucket = &e.Medium
		case "hard":
			bucket = &e.Hard
		}
		if bucket != nil {
			bucket.Attempts++
		}
		if a.IsCorrect {
			e.CorrectAttempts++
			if bucket != nil {
				bucket.Correct++
			}
			if i < recentWindow {
				e.RecentCorrect++
			}
		}
	}

	// The run of correct answers ending at the most recent attempt.
	for _, a := range attempts {
		if !a.IsCorrect {
			break
		}
		e.CorrectStreak++
	}

	const millisecondsPerDay = 86_400_000
	idle := float64(now.UnixMilli()-attempts[0].AttemptedAt.UnixMilli()) / millisecondsPerDay
	e.DaysSinceLastAttempt = ptr(max(0, idle))

	return e
}
