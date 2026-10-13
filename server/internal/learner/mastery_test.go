package learner

import (
	"math"
	"testing"
	"time"
)

// These describe the behaviour promised about mastery, not the exact numbers the current
// weights produce. Tuning the weights should not break them; changing what mastery MEANS
// should.

// solid is a learner with total attempts, correct of them right, all at one difficulty.
func solid(total, correct int, difficulty string) TopicEvidence {
	streak := 0
	if correct == total {
		streak = min(correct, 5)
	}
	e := TopicEvidence{
		TotalAttempts:        total,
		CorrectAttempts:      correct,
		RecentAttempts:       min(total, 10),
		RecentCorrect:        min(correct, 10),
		CorrectStreak:        streak,
		DaysSinceLastAttempt: ptr(0.0),
	}
	b := DifficultyBucket{Attempts: total, Correct: correct}
	switch difficulty {
	case "easy":
		e.Easy = b
	case "hard":
		e.Hard = b
	default:
		e.Medium = b
	}
	return e
}

func TestMasteryStaysInRange(t *testing.T) {
	perfect := ComputeMastery(solid(50, 50, "medium"))
	hopeless := ComputeMastery(solid(50, 0, "medium"))

	if perfect.Score > 100 || hopeless.Score < 0 || perfect.Score <= hopeless.Score {
		t.Fatalf("perfect %v, hopeless %v", perfect.Score, hopeless.Score)
	}
}

func TestMasteryLuckyAnswersAreNotMastery(t *testing.T) {
	lucky := ComputeMastery(solid(3, 3, "medium"))
	proven := ComputeMastery(solid(40, 40, "medium"))

	// Both are 100% accurate, so only the weight of evidence separates them.
	if lucky.Score >= proven.Score || lucky.Score >= 75 {
		t.Fatalf("lucky %v, proven %v", lucky.Score, proven.Score)
	}
}

func TestMasteryGrowsWithEvidence(t *testing.T) {
	prev := -1.0
	for _, n := range []int{3, 10, 30, 80} {
		score := ComputeMastery(solid(n, n, "medium")).Score
		if score <= prev {
			t.Fatalf("n=%d: %v not above %v", n, score, prev)
		}
		prev = score
	}
}

func TestMasteryRewardsHardOverEasy(t *testing.T) {
	easy := ComputeMastery(solid(20, 20, "easy"))
	hard := ComputeMastery(solid(20, 20, "hard"))

	if hard.Score <= easy.Score {
		t.Fatalf("hard %v, easy %v", hard.Score, easy.Score)
	}
}

func TestMasteryIgnoresUntriedDifficulty(t *testing.T) {
	// Never trying a hard question must not look like failing every hard question.
	untried := ComputeMastery(solid(20, 18, "medium"))
	failedHard := ComputeMastery(TopicEvidence{
		TotalAttempts:        25,
		CorrectAttempts:      18,
		RecentAttempts:       10,
		RecentCorrect:        8,
		Medium:               DifficultyBucket{20, 18},
		Hard:                 DifficultyBucket{5, 0},
		DaysSinceLastAttempt: ptr(0.0),
	})

	if untried.Score <= failedHard.Score {
		t.Fatalf("untried %v, failedHard %v", untried.Score, failedHard.Score)
	}
	if untried.Breakdown.DifficultyHandling == nil {
		t.Fatal("difficultyHandling is nil")
	}
}

func TestMasteryWeighsRecentAboveOld(t *testing.T) {
	base := TopicEvidence{
		TotalAttempts:        20,
		CorrectAttempts:      10,
		RecentAttempts:       10,
		Medium:               DifficultyBucket{20, 10},
		DaysSinceLastAttempt: ptr(0.0),
	}
	improving, slipping := base, base
	improving.RecentCorrect = 9
	slipping.RecentCorrect = 1

	// Identical lifetime records, opposite recent form.
	if a, b := ComputeMastery(improving).Score, ComputeMastery(slipping).Score; a <= b {
		t.Fatalf("improving %v, slipping %v", a, b)
	}
}

func TestMasteryDecaysButNeverToNothing(t *testing.T) {
	fresh := ComputeMastery(solid(30, 27, "medium"))
	staleEvidence := solid(30, 27, "medium")
	staleEvidence.DaysSinceLastAttempt = ptr(120.0)
	stale := ComputeMastery(staleEvidence)

	if stale.Score >= fresh.Score {
		t.Fatalf("stale %v, fresh %v", stale.Score, fresh.Score)
	}
	// Decay is capped: a well-learned topic does not become unknown in a month.
	if stale.Score <= fresh.Score*0.8 {
		t.Fatalf("stale %v decayed below 80%% of %v", stale.Score, fresh.Score)
	}
}

func TestMasteryExplainsItself(t *testing.T) {
	b := ComputeMastery(solid(20, 15, "medium")).Breakdown

	if b.RecentAccuracy == nil {
		t.Fatal("recentAccuracy is nil")
	}
	if b.Confidence <= 0 || b.Confidence > 1 || b.DecayFactor > 1 {
		t.Fatalf("breakdown %+v", b)
	}
}

func TestSummariseAttempts(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	attempt := func(correct bool, difficulty string, daysAgo int) AttemptRecord {
		return AttemptRecord{correct, difficulty, now.AddDate(0, 0, -daysAgo)}
	}

	t.Run("empty for a topic never attempted", func(t *testing.T) {
		e := SummariseAttempts(nil, now)
		if e.TotalAttempts != 0 || e.DaysSinceLastAttempt != nil {
			t.Fatalf("%+v", e)
		}
	})

	t.Run("counts totals and fills the difficulty buckets", func(t *testing.T) {
		e := SummariseAttempts([]AttemptRecord{
			attempt(true, "hard", 0),
			attempt(false, "medium", 0),
			attempt(true, "easy", 0),
			attempt(true, "easy", 0),
		}, now)

		if e.TotalAttempts != 4 || e.CorrectAttempts != 3 ||
			e.Easy != (DifficultyBucket{2, 2}) || e.Medium != (DifficultyBucket{1, 0}) || e.Hard != (DifficultyBucket{1, 1}) {
			t.Fatalf("%+v", e)
		}
	})

	t.Run("buckets add up to the total, as the database demands", func(t *testing.T) {
		e := SummariseAttempts([]AttemptRecord{
			attempt(true, "easy", 0), attempt(false, "hard", 0), attempt(true, "medium", 0),
		}, now)
		if e.Easy.Attempts+e.Medium.Attempts+e.Hard.Attempts != e.TotalAttempts {
			t.Fatalf("%+v", e)
		}
	})

	t.Run("streak counts from the newest and stops at the first miss", func(t *testing.T) {
		e := SummariseAttempts([]AttemptRecord{
			attempt(true, "medium", 0), attempt(true, "medium", 0),
			attempt(false, "medium", 0), attempt(true, "medium", 0),
		}, now)
		if e.CorrectStreak != 2 {
			t.Fatalf("streak %d", e.CorrectStreak)
		}
	})

	t.Run("limits the recent window and measures idle days from the newest", func(t *testing.T) {
		attempts := []AttemptRecord{attempt(true, "medium", 3)}
		for range 14 {
			attempts = append(attempts, attempt(false, "medium", 20))
		}
		e := SummariseAttempts(attempts, now)

		if e.TotalAttempts != 15 || e.RecentAttempts != 10 || e.RecentCorrect != 1 {
			t.Fatalf("%+v", e)
		}
		// toBeCloseTo(3, 5): |diff| < 0.5e-5.
		if math.Abs(*e.DaysSinceLastAttempt-3) >= 0.5e-5 {
			t.Fatalf("idle %v", *e.DaysSinceLastAttempt)
		}
	})
}
