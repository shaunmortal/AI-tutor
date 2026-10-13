// Package sim is the offline personalization evaluation (Track D): simulated learners,
// multiple sessions, the app's REAL engines, no database, no AI.
//
// What is real here:
//
//	topic + difficulty  adaptive.PlanSession      (which calls ChooseDifficulty)
//	question            adaptive.SelectQuestions  (fed an in-memory question bank)
//	mastery estimate    learner.SummariseAttempts + ComputeMastery, recomputed from the attempt
//	                    log exactly as practice does after every answer
//
// What is simulated: the learner. Each has a hidden skill per topic (logits). P(correct) is a
// logistic of skill minus question difficulty (1PL IRT). Practice raises skill, more after a
// wrong answer (which the app explains), scaled by item information 4p(1-p) so questions far too
// easy or far too hard teach little. Skill decays towards its starting level between sessions.
//
// Not used: the SRS engine and the plan engine. Neither is consulted by
// GET /recommendations/next, which is the path that picks the next topic and question.
//
// A seeded PRNG drives every random choice, so the same seeds always give the same results.
package sim

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/adaptive"
	"github.com/shaunmortal/AI-tutor/server/internal/learner"
)

// DifficultyLogit is the IRT difficulty each bank level is centred on.
type DifficultyLogit struct {
	Easy   float64 `json:"easy"`
	Medium float64 `json:"medium"`
	Hard   float64 `json:"hard"`
}

func (d DifficultyLogit) of(level adaptive.Difficulty) float64 {
	switch level {
	case adaptive.Easy:
		return d.Easy
	case adaptive.Hard:
		return d.Hard
	}
	return d.Medium
}

// Config is the simulation setup, written to the results file as-is.
type Config struct {
	TopicCount             int             `json:"topicCount"`
	QuestionsPerDifficulty int             `json:"questionsPerDifficulty"` // 15 per topic
	DifficultyLogit        DifficultyLogit `json:"difficultyLogit"`
	Sessions               int             `json:"sessions"`
	DaysBetweenSessions    int             `json:"daysBetweenSessions"` // 10 sessions over 20 days
	QuestionsPerSession    int             `json:"questionsPerSession"`
	Seeds                  []int           `json:"seeds"`
	StartDate              int64           `json:"startDate"` // Unix ms
	WrongAnswerBoost       float64         `json:"wrongAnswerLearningBoost"`
}

// SimConfig is the configuration the published results use.
var SimConfig = Config{
	TopicCount:             8,
	QuestionsPerDifficulty: 5,
	DifficultyLogit:        DifficultyLogit{Easy: -1.2, Medium: 0, Hard: 1.2},
	Sessions:               10,
	DaysBetweenSessions:    2,
	QuestionsPerSession:    15,
	Seeds:                  []int{1, 2, 3, 4, 5},
	StartDate:              time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC).UnixMilli(),
	WrongAnswerBoost:       1.5,
}

const day = 86_400_000

// Mulberry32 is the TypeScript script's PRNG, bit for bit: floats in [0, 1).
func Mulberry32(seed uint32) func() float64 {
	a := seed
	return func() float64 {
		a += 0x6d2b79f5
		t := a
		t = (t ^ (t >> 15)) * (t | 1)
		t ^= t + (t^(t>>7))*(t|61)
		return float64(t^(t>>14)) / 4294967296
	}
}

func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }

func mean(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(max(1, len(xs)))
}

// r2 is Math.round(x*100)/100: halves round towards +Inf, as in JavaScript.
func r2(x float64) float64 {
	y := float64(x * 100) // the conversion stops arm64 fusing this into y-f below (FMA), which breaks ties
	f := math.Floor(y)
	if y-f >= 0.5 {
		f++
	}
	return f / 100
}

// ---------------------------------------------------------------- course

type question struct {
	id         string
	topicID    string
	difficulty adaptive.Difficulty
	b          float64 // IRT difficulty parameter
}

type topic struct{ id, name string }

type course struct {
	topics []topic
	pool   map[string][]question
}

var levels = []adaptive.Difficulty{adaptive.Easy, adaptive.Medium, adaptive.Hard}

// buildCourse is fixed, independent of learner seed, so every run sees the same bank.
func buildCourse() course {
	rng := Mulberry32(424242)
	c := course{pool: map[string][]question{}}
	for i := range SimConfig.TopicCount {
		c.topics = append(c.topics, topic{fmt.Sprintf("t%d", i+1), fmt.Sprintf("Topic %02d", i+1)})
	}
	for _, t := range c.topics {
		var qs []question
		for _, level := range levels {
			for j := range SimConfig.QuestionsPerDifficulty {
				qs = append(qs, question{
					id:         fmt.Sprintf("%s-%c%d", t.id, level[0], j+1),
					topicID:    t.id,
					difficulty: level,
					b:          SimConfig.DifficultyLogit.of(level) + (rng()-0.5)*0.6,
				})
			}
		}
		c.pool[t.id] = qs
	}
	return c
}

// memoryBank is adaptive.QuestionBank in memory, following the same rules as the SQL (filter by
// topic, optional difficulty, exclude ids, limit). The SQL has no ORDER BY, so Postgres returns
// rows in storage order; this returns pool order.
type memoryBank struct {
	c *course
	// Per topic, ids in order of latest attempt (oldest first).
	seen map[string][]string
}

func (m *memoryBank) FindAttemptedIDs(_ context.Context, _, topicID string) ([]string, error) {
	// Most recently answered first, as the real query orders by attempted_at desc.
	ids := slices.Clone(m.seen[topicID])
	slices.Reverse(ids)
	return ids, nil
}

func (m *memoryBank) FindForPractice(_ context.Context, q adaptive.PracticeQuery) ([]adaptive.PracticeQuestion, error) {
	out := []adaptive.PracticeQuestion{}
	for _, x := range m.c.pool[q.TopicID] {
		if len(out) >= q.Limit {
			break
		}
		if (q.Difficulty != "" && x.difficulty != q.Difficulty) || slices.Contains(q.Exclude, x.id) ||
			(q.Only != nil && !slices.Contains(q.Only, x.id)) {
			continue
		}
		out = append(out, adaptive.PracticeQuestion{ID: x.id, TopicID: x.topicID, QuestionType: "mcq",
			Body: x.id, Difficulty: x.difficulty, Marks: 1})
	}
	return out, nil
}

// markSeen moves id to the end, so the order is order of latest attempt.
func (m *memoryBank) markSeen(topicID, id string) {
	m.seen[topicID] = append(slices.DeleteFunc(m.seen[topicID], func(s string) bool { return s == id }), id)
}

// ---------------------------------------------------------------- learners

// Profile is a kind of simulated learner.
type Profile struct {
	Name         string
	initialSkill func(topicIndex int, rng func() float64) float64
	LearningRate float64
	DecayPerDay  float64 // fraction of above-baseline skill lost per idle day
}

func gauss(rng func() float64) float64 {
	u1 := rng()
	u2 := rng()
	return math.Sqrt(-2*math.Log(1-u1)) * math.Cos(2*math.Pi*u2)
}

// Profiles are the simulated learners, in the order runs and summaries list them.
var Profiles = []Profile{
	{Name: "novice", initialSkill: func(_ int, r func() float64) float64 { return -1.6 + 0.3*gauss(r) }, LearningRate: 0.1, DecayPerDay: 0.02},
	// Even topics strong, odd topics weak.
	{Name: "uneven", initialSkill: func(i int, r func() float64) float64 {
		base := -1.6
		if i%2 == 0 {
			base = 1.4
		}
		return base + 0.3*gauss(r)
	}, LearningRate: 0.1, DecayPerDay: 0.02},
	{Name: "forgetful", initialSkill: func(_ int, r func() float64) float64 { return -0.6 + 0.4*gauss(r) }, LearningRate: 0.25, DecayPerDay: 0.12},
	{Name: "advanced", initialSkill: func(_ int, r func() float64) float64 { return 1.6 + 0.3*gauss(r) }, LearningRate: 0.06, DecayPerDay: 0.01},
}

// Policies: the app's adaptive engine, and a uniformly random baseline.
var Policies = []string{"adaptive", "baseline"}

// trueMastery, 0-100, is expected accuracy over the topic's whole bank: the app's score scale.
func trueMastery(skill float64, qs []question) float64 {
	ps := make([]float64, len(qs))
	for i, q := range qs {
		ps[i] = sigmoid(skill - q.b)
	}
	return 100 * mean(ps)
}

// ---------------------------------------------------------------- one run

// SessionPoint is the state after one session.
type SessionPoint struct {
	Session                 int     `json:"session"`
	TrueMastery             float64 `json:"trueMastery"`
	EstimatedMastery        float64 `json:"estimatedMastery"`
	CalibrationError        float64 `json:"calibrationError"`
	WeakTopicTrueMastery    float64 `json:"weakTopicTrueMastery"`
	Accuracy                float64 `json:"accuracy"`
	WithinSessionRepeatRate float64 `json:"withinSessionRepeatRate"`
	CrossSessionRepeatRate  float64 `json:"crossSessionRepeatRate"`
}

// RunResult is one learner, one policy, one seed.
type RunResult struct {
	Profile                 string         `json:"profile"`
	Policy                  string         `json:"policy"`
	Seed                    int            `json:"seed"`
	StartTrueMastery        float64        `json:"startTrueMastery"`
	EndTrueMastery          float64        `json:"endTrueMastery"`
	TrueSkillGain           float64        `json:"trueSkillGain"`
	WeakTopicIDs            []string       `json:"weakTopicIds"`
	WeakTopicGain           float64        `json:"weakTopicGain"`
	InitialCalibrationError float64        `json:"initialCalibrationError"`
	FinalCalibrationError   float64        `json:"finalCalibrationError"`
	WithinSessionRepeatRate float64        `json:"withinSessionRepeatRate"`
	CrossSessionRepeatRate  float64        `json:"crossSessionRepeatRate"`
	Series                  []SessionPoint `json:"series"`
}

// RunOne simulates one learner under one policy.
func RunOne(profile Profile, policy string, seed int) (RunResult, error) {
	c := buildCourse()
	topics, pool := c.topics, c.pool
	// True mastery is measured on the original bank, so questions written later by the top-up
	// do not move the yardstick.
	yardstick := map[string][]question{}
	for id, qs := range pool {
		yardstick[id] = slices.Clone(qs)
	}
	// Topics that ran out of new questions; the app writes FIRST_BATCH (3) more in the
	// background, which become available from the next session. Adaptive policy only.
	type topUp struct {
		topicID    string
		difficulty adaptive.Difficulty
	}
	var pendingTopUps []topUp
	toppedUp := 0

	profileIndex := slices.IndexFunc(Profiles, func(p Profile) bool { return p.Name == profile.Name })
	// Same seed => same initial skills and same answer stream for both policies. Choice
	// randomness (baseline only) gets its own stream so it does not shift the answer stream.
	skillRng := Mulberry32(uint32(seed*1000 + profileIndex))
	answerRng := Mulberry32(uint32(seed*7919 + 17))
	choiceRng := Mulberry32(uint32(seed*104729 + 3))

	baseSkill := map[string]float64{}
	for i, t := range topics {
		baseSkill[t.id] = profile.initialSkill(i, skillRng)
	}
	skill := map[string]float64{}
	attempts := map[string][]learner.AttemptRecord{} // newest first
	for _, t := range topics {
		skill[t.id] = baseSkill[t.id]
	}
	bank := &memoryBank{c: &c, seen: map[string][]string{}}

	tm := func(id string) float64 { return trueMastery(skill[id], yardstick[id]) }
	startTrue := map[string]float64{}
	for _, t := range topics {
		startTrue[t.id] = tm(t.id)
	}
	byStart := slices.Clone(topics)
	slices.SortStableFunc(byStart, func(a, b topic) int {
		if d := startTrue[a.id] - startTrue[b.id]; d != 0 {
			if d < 0 {
				return -1
			}
			return 1
		}
		return strings.Compare(a.id, b.id)
	})
	var weakTopicIDs []string
	for _, t := range byStart[:3] {
		weakTopicIDs = append(weakTopicIDs, t.id)
	}

	overTopics := func(f func(id string) float64) float64 {
		xs := make([]float64, len(topics))
		for i, t := range topics {
			xs[i] = f(t.id)
		}
		return mean(xs)
	}
	overWeak := func(f func(id string) float64) float64 {
		xs := make([]float64, len(weakTopicIDs))
		for i, id := range weakTopicIDs {
			xs[i] = f(id)
		}
		return mean(xs)
	}
	estimate := func(id string, now time.Time) float64 {
		return learner.ComputeMastery(learner.SummariseAttempts(attempts[id], now)).Score
	}
	calibration := func(now time.Time) float64 {
		return overTopics(func(id string) float64 { return math.Abs(estimate(id, now) - tm(id)) })
	}
	at := func(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

	initialCalibrationError := calibration(at(SimConfig.StartDate))
	var series []SessionPoint
	served, withinRepeats, crossRepeats := 0, 0, 0

	for s := range SimConfig.Sessions {
		sessionStart := SimConfig.StartDate + int64(s*SimConfig.DaysBetweenSessions*day)

		// Forgetting since the previous session: above-baseline skill decays towards the start.
		if s > 0 {
			keep := math.Exp(-profile.DecayPerDay * float64(SimConfig.DaysBetweenSessions))
			for _, t := range topics {
				base := baseSkill[t.id]
				skill[t.id] = base + (skill[t.id]-base)*keep
			}
		}

		for _, u := range pendingTopUps {
			for range 3 {
				toppedUp++
				pool[u.topicID] = append(pool[u.topicID], question{
					id: fmt.Sprintf("%s-new%d", u.topicID, toppedUp), topicID: u.topicID,
					difficulty: u.difficulty, b: SimConfig.DifficultyLogit.of(u.difficulty),
				})
			}
		}
		pendingTopUps = nil

		seenBeforeSession := map[string]bool{}
		for _, ids := range bank.seen {
			for _, id := range ids {
				seenBeforeSession[id] = true
			}
		}
		seenThisSession := map[string]bool{}
		answered, correctCount, sessionWithin, sessionCross := 0, 0, 0, 0

		answer := func(q question) {
			now := at(sessionStart + int64(answered)*60_000)
			before := skill[q.topicID]
			p := sigmoid(before - q.b)
			correct := answerRng() < p
			boost := 1.0
			if !correct {
				boost = SimConfig.WrongAnswerBoost
			}
			skill[q.topicID] = before + profile.LearningRate*boost*4*p*(1-p)

			if seenThisSession[q.id] {
				sessionWithin++
			} else if seenBeforeSession[q.id] {
				sessionCross++
			}
			seenThisSession[q.id] = true
			bank.markSeen(q.topicID, q.id)
			attempts[q.topicID] = slices.Insert(attempts[q.topicID], 0, learner.AttemptRecord{
				IsCorrect: correct, Difficulty: string(q.difficulty), AttemptedAt: now,
			})
			answered++
			if correct {
				correctCount++
			}
		}

		for answered < SimConfig.QuestionsPerSession {
			remaining := SimConfig.QuestionsPerSession - answered

			if policy == "baseline" {
				t := topics[int(math.Floor(choiceRng()*float64(len(topics))))]
				qs := pool[t.id]
				answer(qs[int(math.Floor(choiceRng()*float64(len(qs))))])
				continue
			}

			// ADAPTIVE: rebuild candidates from the log the way the topic repository reads
			// concept_mastery, then make the same two calls the recommendations route makes.
			now := at(sessionStart + int64(answered)*60_000)
			candidates := make([]adaptive.TopicCandidate, len(topics))
			for i, t := range topics {
				candidates[i] = adaptive.TopicCandidate{TopicID: t.id, Name: t.name, Weight: 1}
				log := attempts[t.id]
				if len(log) == 0 {
					continue
				}
				e := learner.SummariseAttempts(log, now)
				score := learner.ComputeMastery(e).Score
				accuracy := float64(e.RecentCorrect) / float64(e.RecentAttempts)
				candidates[i].MasteryScore = &score
				candidates[i].RecentAccuracy = &accuracy
				candidates[i].RecentAttempts = e.RecentAttempts
				candidates[i].TotalAttempts = e.TotalAttempts
				candidates[i].DaysSinceLastAttempt = e.DaysSinceLastAttempt
			}

			plan := adaptive.PlanSession(adaptive.RecommendationInput{Candidates: candidates})
			topicID := plan.Candidate.TopicID
			selection, err := adaptive.SelectQuestions(context.Background(), bank, adaptive.SelectOptions{
				UserID: "sim", TopicID: topicID, Difficulty: plan.Difficulty,
				Count: min(plan.RequestedQuestionCount, remaining),
			})
			if err != nil {
				return RunResult{}, err
			}
			if len(selection.Questions) == 0 {
				return RunResult{}, fmt.Errorf("empty selection for %s", topicID)
			}
			if selection.Repeats > 0 && !slices.ContainsFunc(pendingTopUps, func(u topUp) bool { return u.topicID == topicID }) {
				pendingTopUps = append(pendingTopUps, topUp{topicID, plan.Difficulty})
			}

			byID := map[string]question{}
			for _, q := range pool[topicID] {
				byID[q.id] = q
			}
			for _, q := range selection.Questions {
				answer(byID[q.ID])
			}
		}

		served += answered
		withinRepeats += sessionWithin
		crossRepeats += sessionCross

		end := at(sessionStart + int64(answered)*60_000)
		n := float64(answered)
		series = append(series, SessionPoint{
			Session:                 s + 1,
			TrueMastery:             r2(overTopics(tm)),
			EstimatedMastery:        r2(overTopics(func(id string) float64 { return estimate(id, end) })),
			CalibrationError:        r2(calibration(end)),
			WeakTopicTrueMastery:    r2(overWeak(tm)),
			Accuracy:                r2(float64(correctCount) / n),
			WithinSessionRepeatRate: r2(float64(sessionWithin) / n),
			CrossSessionRepeatRate:  r2(float64(sessionCross) / n),
		})
	}

	startTrueMastery := overTopics(func(id string) float64 { return startTrue[id] })
	endTrueMastery := overTopics(tm)

	return RunResult{
		Profile:                 profile.Name,
		Policy:                  policy,
		Seed:                    seed,
		StartTrueMastery:        r2(startTrueMastery),
		EndTrueMastery:          r2(endTrueMastery),
		TrueSkillGain:           r2(endTrueMastery - startTrueMastery),
		WeakTopicIDs:            weakTopicIDs,
		WeakTopicGain:           r2(overWeak(func(id string) float64 { return tm(id) - startTrue[id] })),
		InitialCalibrationError: r2(initialCalibrationError),
		FinalCalibrationError:   series[len(series)-1].CalibrationError,
		WithinSessionRepeatRate: r2(float64(withinRepeats) / float64(served)),
		CrossSessionRepeatRate:  r2(float64(crossRepeats) / float64(served)),
		Series:                  series,
	}, nil
}

// ---------------------------------------------------------------- aggregate

// Summary is one profile under one policy, averaged over seeds.
type Summary struct {
	Profile                 string  `json:"profile"`
	Policy                  string  `json:"policy"`
	TrueSkillGain           float64 `json:"trueSkillGain"`
	WeakTopicGain           float64 `json:"weakTopicGain"`
	InitialCalibrationError float64 `json:"initialCalibrationError"`
	FinalCalibrationError   float64 `json:"finalCalibrationError"`
	WithinSessionRepeatRate float64 `json:"withinSessionRepeatRate"`
	CrossSessionRepeatRate  float64 `json:"crossSessionRepeatRate"`
	// Per-session means across seeds, for charts.
	Series []SessionPoint `json:"series"`
}

// ProfileInfo is a profile's tunables, as the results file lists them.
type ProfileInfo struct {
	Name         string  `json:"name"`
	LearningRate float64 `json:"learningRate"`
	DecayPerDay  float64 `json:"decayPerDay"`
}

// ResultConfig is Config plus the profiles.
type ResultConfig struct {
	Config
	Profiles []ProfileInfo `json:"profiles"`
}

// Result is the whole results file.
type Result struct {
	Config    ResultConfig `json:"config"`
	Summaries []Summary    `json:"summaries"`
	Runs      []RunResult  `json:"runs"`
}

// Run simulates every profile under every policy for every seed and averages over seeds.
func Run(seeds []int) (Result, error) {
	var runs []RunResult
	for _, p := range Profiles {
		for _, policy := range Policies {
			for _, seed := range seeds {
				r, err := RunOne(p, policy, seed)
				if err != nil {
					return Result{}, err
				}
				runs = append(runs, r)
			}
		}
	}

	var summaries []Summary
	for _, p := range Profiles {
		for _, policy := range Policies {
			var group []RunResult
			for _, r := range runs {
				if r.Profile == p.Name && r.Policy == policy {
					group = append(group, r)
				}
			}
			avg := func(f func(r RunResult) float64) float64 {
				xs := make([]float64, len(group))
				for i, r := range group {
					xs[i] = f(r)
				}
				return r2(mean(xs))
			}
			series := make([]SessionPoint, len(group[0].Series))
			for i := range series {
				m := func(f func(p SessionPoint) float64) float64 {
					return avg(func(r RunResult) float64 { return f(r.Series[i]) })
				}
				series[i] = SessionPoint{
					Session:                 i + 1,
					TrueMastery:             m(func(p SessionPoint) float64 { return p.TrueMastery }),
					EstimatedMastery:        m(func(p SessionPoint) float64 { return p.EstimatedMastery }),
					CalibrationError:        m(func(p SessionPoint) float64 { return p.CalibrationError }),
					WeakTopicTrueMastery:    m(func(p SessionPoint) float64 { return p.WeakTopicTrueMastery }),
					Accuracy:                m(func(p SessionPoint) float64 { return p.Accuracy }),
					WithinSessionRepeatRate: m(func(p SessionPoint) float64 { return p.WithinSessionRepeatRate }),
					CrossSessionRepeatRate:  m(func(p SessionPoint) float64 { return p.CrossSessionRepeatRate }),
				}
			}
			summaries = append(summaries, Summary{
				Profile:                 p.Name,
				Policy:                  policy,
				TrueSkillGain:           avg(func(r RunResult) float64 { return r.TrueSkillGain }),
				WeakTopicGain:           avg(func(r RunResult) float64 { return r.WeakTopicGain }),
				InitialCalibrationError: avg(func(r RunResult) float64 { return r.InitialCalibrationError }),
				FinalCalibrationError:   avg(func(r RunResult) float64 { return r.FinalCalibrationError }),
				WithinSessionRepeatRate: avg(func(r RunResult) float64 { return r.WithinSessionRepeatRate }),
				CrossSessionRepeatRate:  avg(func(r RunResult) float64 { return r.CrossSessionRepeatRate }),
				Series:                  series,
			})
		}
	}

	cfg := ResultConfig{Config: SimConfig}
	cfg.Seeds = seeds
	for _, p := range Profiles {
		cfg.Profiles = append(cfg.Profiles, ProfileInfo{p.Name, p.LearningRate, p.DecayPerDay})
	}
	return Result{Config: cfg, Summaries: summaries, Runs: runs}, nil
}

// Table is the summary as a fixed-width text table, for the terminal.
func Table(summaries []Summary) string {
	pct := func(x float64) string { return fmt.Sprintf("%.1f%%", x*100) }
	f2 := func(x float64) string { return fmt.Sprintf("%.2f", x) }
	cols := []string{"profile", "policy", "skill gain", "weak-topic gain", "calib err s1", "calib err s5",
		"calib err s10", "repeat in-session", "repeat cross-session"}
	rows := [][]string{}
	for _, s := range summaries {
		rows = append(rows, []string{s.Profile, s.Policy, f2(s.TrueSkillGain), f2(s.WeakTopicGain),
			f2(s.Series[0].CalibrationError), f2(s.Series[4].CalibrationError), f2(s.FinalCalibrationError),
			pct(s.WithinSessionRepeatRate), pct(s.CrossSessionRepeatRate)})
	}
	width := make([]int, len(cols))
	for i, c := range cols {
		width[i] = len(c)
		for _, r := range rows {
			width[i] = max(width[i], len(r[i]))
		}
	}
	line := func(cells []string) string {
		out := make([]string, len(cells))
		for i, c := range cells {
			out[i] = c + strings.Repeat(" ", width[i]-len(c))
		}
		return strings.Join(out, " | ")
	}
	var b strings.Builder
	b.WriteString(line(cols) + "\n")
	dashes := make([]string, len(cols))
	for i, w := range width {
		dashes[i] = strings.Repeat("-", w)
	}
	b.WriteString(strings.Join(dashes, "-|-") + "\n")
	for _, r := range rows {
		b.WriteString(line(r) + "\n")
	}
	return b.String()
}
