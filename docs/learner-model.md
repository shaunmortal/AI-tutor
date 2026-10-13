# Learner model

What the app believes a learner knows, how every answer changes that belief, and how it decides
what to do next. All of it is deterministic Go with no model calls, so the same inputs always give
the same output and every number can be checked by hand.

| Piece | Code |
|---|---|
| Mastery | `server/internal/learner/mastery.go` |
| Spaced repetition (SM-2) | `learner/srs.go`, `learner/flashcard.go` |
| Self-rated priors | `learner/prior.go`, `store/onboarding.go` |
| Habits (streak, consistency, pace) | `learner/habit.go` |
| Difficulty, next action, question selection | `adaptive/difficulty.go`, `recommendation.go`, `selector.go` |
| Diagnostic, mock tests, grading | `assessment/diagnostic.go`, `mocktest.go`, `grader.go` |
| Study plan and re-planning | `planning/plan.go`, `planning/replan.go` |
| Readiness | `readiness/readiness.go` |
| Simulated students | `internal/sim`, `cmd/simulate` |

## Evidence: the attempt log

Every answer is one row in `question_attempts`. The row records the question, whether it was
correct, the time taken and the `source`: `practice`, `assessment`, `topic_test`, `mock_test`,
`revision` or `tutor`. The log is never edited. After each answer, mastery for the touched topic is
**recomputed from the whole log** (`learner.SummariseAttempts` → `ComputeMastery`) and cached in
`concept_mastery`. It is never nudged in place, so the cache cannot drift from the history it
summarises.

Every path that grades an answer goes through the same recorder (`web/practice` `recordAnswers` /
`RecordGraded`), inside one transaction:

1. Insert the attempt.
2. If the answer showed a misconception, upsert it in `mistake_patterns`.
3. Recompute mastery for each topic touched.
4. Move the SM-2 revision schedule. This runs in a savepoint, so a failure here never loses the answer.
5. Update the learner profile: totals, streak, consistency, average seconds per question.

| Where the answer comes from | Route | `source` |
|---|---|---|
| Practice question | `POST /attempts` | `practice` (or the value sent) |
| Diagnostic paper | `POST /assessments/{id}/submit` | `assessment` |
| Mock test | `POST /mock-tests/{id}/submit` | `mock_test` |
| Tutor quick check | `POST /ai/checks/{id}/answer` | `tutor` |
| Flashcard ("known" / "again") | `POST /revision/flashcards/{id}/review` | Moves the SM-2 schedule only. No attempt is recorded. |

## Mastery (0–100 per topic)

`ComputeMastery(TopicEvidence)`:

| Component | Weight | Definition |
|---|---|---|
| Recent accuracy | **0.40** | Correct share of the latest `recentWindow` = 10 attempts |
| Historical accuracy | **0.25** | Correct share of all attempts |
| Difficulty handling | **0.25** | Difficulty-weighted accuracy × level factor (below) |
| Consistency | **0.10** | `min(1, correctStreak / 5)`, where the streak is the run of correct answers ending at the latest |

**Difficulty handling.**

- Bucket weights are easy 1, medium 2, hard 3.
- `weightedAccuracy = Σ w·(correct/attempts) / Σ w`, over buckets that have attempts only.
- `attemptedLevel = Σ w·attempts / (attempts × 3)`. That is 1/3 for easy-only work and 1 for hard-only work.
- `levelFactor = 0.6 + 0.4 × attemptedLevel`. The 0.6 is `difficultyLevelFloor`.
- `difficultyHandling = weightedAccuracy × levelFactor`. An all-correct, easy-only record therefore cannot reach full marks.

**Raw score.** Components with no evidence are dropped and the remaining weights are rescaled:
`raw = Σ wᵢ·vᵢ / Σ wᵢ`. With no evidence at all, `raw = 0.35`.

**Shrinkage** towards a neutral prior of **0.35** (`neutralPrior`):

```
confidence = n / (n + 5)                       n = total attempts (evidenceHalfWeight = 5)
shrunk     = raw × confidence + 0.35 × (1 − confidence)
```

Confidence is 0.5 at 5 attempts and 0.8 at 20, so three lucky answers do not read as mastery.

**Decay** for a topic left alone, capped:

```
decayFactor = 1 − min(idleDays / 30, 1) × 0.15     (decayAfterDays = 30, maxDecay = 0.15)
score       = clamp(shrunk × decayFactor × 100, 0, 100), rounded to 2 dp
```

The breakdown (`recentAccuracy`, `historicalAccuracy`, `difficultyHandling`, `consistency`,
`rawScore`, `confidence`, `decayFactor`) is returned with the score so a screen can explain it.

## Self-rated priors (new learners)

At onboarding, the learner rates their confidence from 1 to 5 in each area: a subject, or a single
topic (`PUT /learner/confidence`, stored in `self_ratings`). For a topic with **no attempts**, the
planner and the recommendation engine use the rating as the mastery estimate. A topic's own rating
wins over its subject's.

| Confidence | 1 | 2 | 3 | 4 | 5 |
|---|---|---|---|---|---|
| Prior mastery (`SelfRatedPrior`) | 15 | 25 | 35 | 50 | 65 |

- 3 equals the neutral 35, so "average" changes nothing.
- The top value, 65, stays below "solid" (75) and the planner's target (85). A self-rating can make an area less urgent but can never mark it done.
- The first real answer replaces the prior entirely, because mastery is computed from attempts only.

The onboarding flow in the web app runs: intake (course, exam date, daily minutes, language,
confidence) → upload material → automatic course map → diagnostic → plan.

## Diagnostic

`assessment.PlanDiagnostic` aims for breadth: one question per topic, as many topics as the paper allows.

| Setting | Value |
|---|---|
| Questions | default 15, min 6, max 25 |
| Per subject / per topic | at most 4 / at most 1 |
| Time allowance | 75 s per question |
| Selection | Round-robin over subjects, heaviest subject first. Within a subject, the topic with the deepest verified bank goes first. A subject with no measurable topic is named in `uncovered`. |

Answers are recorded as attempts (`source = assessment`), so they move mastery like any other
answer. The result screen (`SummariseDiagnostic`) puts each subject in a coarse band:

- **strong** at 75 % or more;
- **weak** below 45 %;
- **moderate** in between;
- **unmeasured** with no questions.

These bands never write mastery. A skipped question counts as wrong for the score but is reported separately.

## Spaced repetition (SM-2, per topic, with an exam cap)

`learner.NextReview` follows SM-2, with the review grade taken from **measured accuracy** across
the review's questions instead of a self-reported 0–5.

| Constant | Value |
|---|---|
| Starting ease | 2.5 |
| Ease step | ±0.15, clamped to [1.3, 3.0] |
| Pass | accuracy ≥ 80 % |
| Lapse | accuracy < 50 % |
| Shaky middle (50–80 %) | interval × 1.2, ease unchanged |
| First interval / second interval | 1 day / 3 days |
| Max interval | 60 days |
| Full credit needs | ≥ 3 questions in the review. Fewer means a pass only steps the interval × 1.2. |
| Exam buffer | 1 day: nothing is scheduled for the day of the exam |

Rules:

- **First study** (`FirstSchedule`): due in 1 day, ease 2.5.
- **Lapse:** interval goes back to 1 day, ease −0.15, `lapses + 1`. Ease falls even when the evidence is thin.
- **Pass:**
  - first review → 3 days;
  - later reviews → `round(interval × ease)`, then ease +0.15;
  - thin evidence → `round(interval × 1.2)`.
- **Exam cap.** `room = daysUntilExam − 1`:
  - if `room < 1`, the topic is reported `finishedForThisGoal` rather than scheduled after the exam;
  - otherwise the interval is shortened to fit inside `room`.
- **Practice moves a topic's schedule at most once a day.** A second batch on the same day can only collapse the interval (accuracy under 50 %).
- **Flashcards** (`FlashcardReview`) count as a one-question review: "known" = 100 %, "again" = 0 %. A second "known" on the same day changes nothing.

`GET /revision/due` lists overdue topics, longest overdue first (`DaysOverdue`). The plan puts them
before new learning (below).

## Misconceptions

A wrong answer can show a specific misunderstanding:

- **Choice questions.** Each generated wrong option carries a label in `questions.option_misconceptions`, for example "confuses mass with weight". Choosing that option records the label. This column is never granted to learners, because its blanks would reveal which options are correct.
- **Short answers.** The rubric grader (`ShortAnswerGraderSystem`) names a misconception when there is a specific one, and null for a blank, a guess or a slip.

Each one is upserted into `mistake_patterns`, unique on `(user_id, topic_id, lower(misconception))`:
`occurrences + 1`, `last_seen_at`, and the evidence attempt ids. `GET /misconceptions` shows the
weakest topics first, with up to 3 misconceptions each. The mock test report lists misconceptions
for topics scored under 60 %, and the tutor's prompt includes the learner's recent mistakes on the
topic.

## Difficulty (`adaptive.ChooseDifficulty`)

| Mastery | Base difficulty |
|---|---|
| < 40 | easy |
| 40–69 | medium |
| ≥ 70 | hard |

Recent form then overrides the base level, but only with at least 4 recent attempts:

- recent accuracy < 0.40 → step back one level;
- recent accuracy > 0.85 → step up one level.

The reason is returned in words.

## Next-best action (`GET /recommendations/next`)

`adaptive.PlanSession` ranks the topics in the goal's scope.

**Priority**

```
gap       = (100 − mastery) / 100     (mastery = measured, else self-rated prior, else gap 1)
staleness = min(1, daysSinceLastAttempt / 14)   (never attempted = 1)
priority  = (0.7 × gap + 0.3 × staleness) × topic weight
```

**Rules**

- **Finish what you started.** While any started topic is below 50 mastery, only started topics compete.
- **Weak prerequisite first** (`WeakPrerequisiteFirst`). If the winner has a prerequisite in scope with effective mastery below 60, the engine follows the chain down and recommends that prerequisite instead. The reason then ends with "It comes before X, which builds on it."
- **Action:**
  - no attempts → `learn_new` (3 questions);
  - mastery ≥ 75 and idle ≥ 10 days → `revise` (4);
  - mastery ≥ 75 → `mini_test` (10);
  - otherwise → `practice` (5).
- **Time fit:** with `?minutes=`, the count is cut to `floor(minutes × 60 / secondsPerQuestion)`. The pace is the measured one, or 90 s by default.
- Two runners-up are returned as `alternatives`. Ties break on name.

**Question selection** (`adaptive.SelectQuestions`), in order:

1. unseen questions at the chosen difficulty;
2. unseen questions at the nearest other level (easy → medium → hard, medium → easy → hard, hard → medium → easy);
3. questions seen before, the ones answered longest ago first.

Repeats are counted and reported. When the bank is empty, up to 3 questions are generated and
verified on the spot. When the selection had to repeat, more are generated in the background.

## Study plan (`planning.BuildPlan`)

**Inputs**

- days remaining (start date up to the day before the exam);
- daily minutes (10–960);
- topics with mastery, else self-rated prior, else `UnknownMastery` = 35;
- due revisions;
- prerequisites from the course map;
- the learner's pace.

**Need per topic**

```
need = max(0.15, max(0, 85 − mastery) / 85) × max(0.1, weight)       MasteryTarget = 85
```

**Algorithm**

1. **Budget:** `days × min(dailyMinutes, 4 × 60)`.
2. **Allocate:** each topic's share is `round(need / Σneed × budget)`, rounded down to whole sessions of `sessionFloor = min(15, perDay)` minutes. A topic that cannot get one session is listed in `omittedTopics` with the reason.
3. **Revision first.** Due revisions take 15-minute sessions, longest overdue first, on the earliest days. They use at most 50 % of any day.
4. **Lay out learning** day by day, round-robin. The starting topic rotates each day.
   - At most 4 topics a day.
   - Session cap is `min(60, max(floor, perDay / 2))`.
   - A leftover stub shorter than the floor is folded into the session when the day has room.
5. **Prerequisite order.** A topic's first learning session waits until each of its *weak* prerequisites in the plan (mastery < 60) has had one, either on an earlier day or earlier the same day. A third pass ignores prerequisites when nothing could be placed, so a cycle in the course map cannot stall the plan.
6. **Question counts** are `max(1, round(minutes × 60 / secondsPerQuestion))`, with 90 s as the fallback pace.

Every session carries a reason in words, such as "X is at mastery 32 from 6 attempts, so it needs
the most time."

**Re-planning** (`planning.ShouldReplan`) runs when the plan is opened (`GET /study-plan/current`):

1. Past sessions are first settled from the attempts actually recorded that day (`ReconcileStatus`):
   - no answers → `missed`;
   - at least the planned number → `completed`;
   - anything in between → `partial`.
2. **Rate limit:** no rebuild within 20 hours of the current plan's creation.
3. **Missed sessions:** 3 or more missed in the last 7 days → rebuild with reason `missed_sessions`.
4. **Poor performance:** 2 or more topics with at least 4 recent attempts and recent accuracy under 35 % → rebuild with reason `poor_performance`.
5. `POST /study-plan/generate` rebuilds on request (reason `requested`, limited to 20 an hour).

Plans are versioned. A rebuild supersedes the old plan, and the learner is told why.

## Mock tests

`assessment.PlanMockTest` takes its topics from the active plan, or from the topics the learner
picks (`topicIds`), or else from the goal.

| Setting | Value |
|---|---|
| Questions | default 15, min 5, max 30 |
| Topics | at most 6, each giving at least 2 questions |
| Share | `max(0.2, (100 − mastery)/100) × (1 + 0.5 × min(3, plannedSessions)) × max(0.2, weight)` |
| Time | the learner's pace, or 100 s per question |
| Pass mark | 60 % |

Skipped questions score zero but are reported separately. The review shows each question with the
correct answer, the explanation, the misconception if any, and the source excerpt.

## Exam readiness (`GET /readiness`)

`readiness.Score`. Components that cannot be measured are dropped and the rest are reweighted, so
the total is always out of 100.

| Component | Weight | Score |
|---|---|---|
| Mastery | 0.45 | Weighted mean mastery ÷ 85 × 100. A never-attempted topic counts as 0 here, because silence is not evidence. |
| Coverage | 0.20 | Share of exam weight attempted at all |
| Mock | 0.25 | Mean of the last 3 finished mock tests. Omitted if none. |
| Time | 0.10 | `available / needed × 100`, where needed = Σ weight·max(0, 85 − mastery) × 1.2 min and available = days × daily minutes. 100 when there is no gap. Omitted when the exam is today or past. |

| Output | Rule |
|---|---|
| Bands | ready ≥ 75, on track ≥ 55, building ≥ 30, otherwise not ready |
| Confidence | low under 25 attempts; high at 120 or more attempts with at least one mock; medium otherwise |

The result also carries:

- `gaps`: attempted topics short of the target, ranked by weight × distance;
- `notStarted`: the heaviest untouched topics;
- a one-sentence verdict naming the weakest component.

## Habits

`learner/habit.go`:

- **Streak:** unchanged if the learner already practised today, +1 if they last practised yesterday, otherwise reset to 1.
- **Consistency:** distinct practice days ÷ days since the first answer (today included), capped at 100 %.
- **Pace:** average seconds per question. It feeds plan and recommendation question counts.

Badges and the activity calendar (`learner/badge.go`, `activity.go`) are built from the same log.

## Simulated students

### Method

`server/internal/sim`, run with `cd server && go run ./cmd/simulate`. The app's **real** engines
run against simulated learners, with no database and no AI:

| Real (from the app) | Simulated |
|---|---|
| Topic and difficulty: `adaptive.PlanSession` (calls `ChooseDifficulty`) | Each learner has a hidden skill per topic, as a logit |
| Questions: `adaptive.SelectQuestions` over an in-memory bank | P(correct) = sigmoid(skill − difficulty), a 1-parameter IRT model. Difficulty logits are easy −1.2, medium 0, hard +1.2. |
| Mastery estimate: `SummariseAttempts` + `ComputeMastery` after every answer | Learning: skill rises by learning rate × item information 4p(1−p), × 1.5 after a wrong answer. Between sessions, above-baseline skill is multiplied by e^(−decay × days). |

**Setup**

- 8 topics with 5 questions per difficulty.
- 10 sessions, 2 days apart, 15 questions each.
- Seeds 1–5. A seeded PRNG (Mulberry32) makes runs reproducible.
- The SRS and plan engines are not used, because `GET /recommendations/next` does not consult them.
- The baseline policy picks a uniformly random topic and question.

| Profile | Starting skill | Learning rate | Decay / day |
|---|---|---|---|
| novice | −1.6 ± 0.3 | 0.10 | 0.02 |
| uneven | +1.4 (even topics) / −1.6 (odd), ± 0.3 | 0.10 | 0.02 |
| forgetful | −0.6 ± 0.4 | 0.25 | 0.12 |
| advanced | +1.6 ± 0.3 | 0.06 | 0.01 |

**Metrics**

- **True mastery:** expected accuracy over the topic's whole bank, on a 0–100 scale.
- **Gain:** the change in true mastery, overall and over the 3 weakest starting topics.
- **Estimate error:** the mean |estimated − true| mastery at the end.
- **Repeat rates:** within a session and across sessions.

### Results

From `eval/results/simulation-results.json`, averaged over 5 seeds:

| Profile | Overall gain (adaptive / random) | Weak-topic gain (adaptive / random) | Mastery estimate error at end (adaptive / random) |
|---|---|---|---|
| Novice | **26.8** / 25.5 | **31.7** / 21.9 | **12.5** / 14.6 |
| Uneven | **23.7** / 18.2 | **46.1** / 24.9 | **15.5** / 15.9 |
| Forgetful | 29.9 / **32.5** | 36.2 / 36.4 | **11.9** / 19.7 |
| Advanced | 6.9 / 7.1 | 9.2 / 9.1 | 16.3 / 15.5 |

Repeats within a session were 0 % with the adaptive policy and 6 % with the random baseline.

**Reading:** the adaptive policy gives large gains on weak topics for novice and uneven learners. It
shows no advantage yet for the forgetful and advanced profiles.
