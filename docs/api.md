# API reference

Base URL: `/api/v1`. This reference was built from the route tables:

- `routes()` in `server/internal/web/learning/learning.go`
- `routes()` in `server/internal/web/practice/practice.go`
- `svc.routes()` in `server/internal/web/content/content.go`
- the two health routes in `server/cmd/api/main.go`

## Conventions

**Auth.** Every route except the health checks requires `Authorization: Bearer <Supabase access token>`.
The learner's id comes only from the token.

**Envelope.** Every response has this shape:

```json
{ "success": true,  "data": { ... }, "error": null }
{ "success": false, "data": null, "error": { "code": "…", "message": "…" } }
```

**Bodies.**

- JSON bodies are capped at 100 KiB.
- Most routes ignore unknown keys. `PUT /learner/confidence` and the `/sources` and `/ai` JSON routes reject them.
- An empty body on a route that expects an object is a 400. Send `{}` when every field is optional.
- Validation messages are `field: message`.

**Rate limits.** Fixed one-hour windows per learner, kept in Redis. If Redis is down, requests are allowed.

**Errors on every route**

| Status | Code | When |
|---|---|---|
| 400 | `INVALID_INPUT` | Body, query or path id failed validation |
| 401 | `UNAUTHENTICATED` | Missing, malformed, invalid or expired token |
| 429 | `RATE_LIMITED` | Hourly quota used up. The message says how many seconds to wait. |
| 503 | `SERVER_BUSY` | The instance is at `MAX_IN_FLIGHT`. Sent with `Retry-After: 2`. |
| 500 | `INTERNAL_ERROR` | Anything unexpected. The details are logged on the server, not returned. |
| 404 | `NOT_FOUND` | No such route |

**AI errors**, on any route that calls the model:

| Status | Code |
|---|---|
| 503 | `AI_UNAVAILABLE` (no key configured, or provider down) |
| 503 | `AI_REFUSED` |
| 503 or 502 | `AI_MALFORMED` |
| 504 | `AI_TIMEOUT` |
| 429 | `AI_RATE_LIMITED` |

**Storage errors**, on routes that use Storage: 503 `STORAGE_UNAVAILABLE` when `SUPABASE_SECRET_KEY` is not set.

## Summary

| Method | Path | Rate limit / hour | Purpose |
|---|---|---|---|
| GET | `/health` | — (no auth) | Liveness |
| GET | `/health/deep` | — (no auth) | Database and Redis reachability |
| GET | `/goals/subjects` | | Subjects of a curriculum |
| GET | `/goals` | | Active goal |
| POST | `/goals` | | Create goal (archives the old one) |
| GET | `/curricula/default` | | Built-in syllabus |
| POST | `/curricula/resolve` | 20 | Turn free text into a syllabus (AI) |
| GET | `/learner/summary` | | Dashboard numbers |
| GET | `/learner/activity` | | Calendar and badges |
| GET | `/learner/subjects` | | Progress per subject |
| GET | `/learner/profile` | | Profile |
| PATCH | `/learner/profile` | | Update name, timezone, language |
| GET | `/learner/confidence` | | Self-ratings |
| PUT | `/learner/confidence` | | Save self-ratings |
| GET | `/study-plan/current` | | Current plan (may re-plan) |
| POST | `/study-plan/generate` | 20 | Rebuild plan |
| GET | `/readiness` | | Exam readiness |
| GET | `/revision/due` | | Topics due for review |
| GET | `/revision/flashcards` | | Flashcard decks for due and weak topics (AI on first use) |
| POST | `/revision/flashcards/{id}/review` | | Mark a card known / again |
| GET | `/groups` | | My groups |
| POST | `/groups` | | Create group |
| POST | `/groups/join` | 30 | Join by invite code |
| GET | `/groups/{id}` | | Group and member comparison |
| POST | `/groups/{id}/leave` | | Leave |
| POST | `/attempts` | | Answer one question |
| GET | `/recommendations/next` | | Next-best action and its questions |
| POST | `/questions/generate` | 10 | Generate and verify questions (AI) |
| GET | `/assessments/diagnostic` | | Open diagnostic, if any |
| POST | `/assessments/diagnostic` | 10 | Start a diagnostic |
| GET | `/assessments/{id}` | | One assessment |
| POST | `/assessments/{id}/submit` | | Submit a diagnostic |
| GET | `/mock-tests` | | History, open test, topics |
| POST | `/mock-tests` | 20 | Build a mock test |
| GET | `/mock-tests/{id}` | | One test (with review once submitted) |
| POST | `/mock-tests/{id}/submit` | | Submit a test |
| GET | `/misconceptions` | | Misconceptions by topic |
| GET | `/sources` | | My material and course-map status |
| POST | `/sources/upload` | 30 (shared with the two video routes) | Upload PDF / PPTX |
| POST | `/sources/video` | 30 (shared) | Add a YouTube lecture |
| POST | `/sources/video/captions` | 30 (shared) | Add a lecture with a .vtt/.srt file |
| POST | `/sources/organize` | 10 | Build the course map (AI) |
| GET | `/sources/map/{curriculumId}` | | Course map |
| GET | `/sources/excerpts/{id}/open` | | Link to the exact page, slide or second |
| DELETE | `/sources/{id}` | | Delete a source |
| GET | `/ai/conversations` | | Tutor threads |
| GET | `/ai/conversations/{id}` | | One thread |
| POST | `/ai/tutor` | 40 | Ask the tutor (AI) |
| POST | `/ai/checks/{id}/answer` | | Answer the tutor's quick check |

## Health

**GET /health** → 200 `{status: "ok", uptime}`. This route is never shed.

**GET /health/deep** → 200 `{status: "ok", database: "reachable", redis: bool, latencyMs, uptime}`.
Returns 503 `DATABASE_UNREACHABLE` if the database does not answer.

## Goals and curricula

**GET /goals/subjects?curriculumId=uuid** → `{subjects: [{id, name, weight, topicCount}]}`

**GET /goals** → `{goal: Goal | null}`

`Goal` = `{id, title, examDate, dailyMinutes, subjectIds[], curriculumId, daysRemaining, totalMinutesAvailable}`.

**POST /goals** → 201 `{goal}`

| Field | Rule |
|---|---|
| `title` | string, 1–120 after trim |
| `curriculumId` | uuid |
| `examDate` | `YYYY-MM-DD`, a real date, 1–730 days ahead |
| `dailyMinutes` | int, 10–960 |
| `subjectIds` | 1–20 uuids. Duplicates are removed. |

**GET /curricula/default** → `{status: "cached", curriculum: {id, slug, name, description, isAiGenerated}, subjects}`.
Returns 500 `NO_DEFAULT_CURRICULUM` if the seed is missing.

**POST /curricula/resolve** `{goalText: 2–120}` → 200 with one of:

- `{status: "cached" | "created", curriculum, subjects}`
- `{status: "rejected", message}`, when the text is not something one can study for.

AI errors apply.

## Learner

**GET /learner/summary** → `{hasActivity, attempts{total, correct, accuracyPercent}, habits{currentStreakDays, longestStreakDays, consistencyPercent, lastActiveDate, avgSecondsPerQuestion}, preferredDifficulty, weakestTopics[{topicId, name, masteryScore, attempts, accuracyPercent, recentAccuracyPercent}], computedAt}`

**GET /learner/activity** → `{calendar{days[{date, count, level 0–4}], totalAnswered, activeDays, maxStreak, currentStreak, from, to}, badges[{id, name, family, description, earnedOn}], upcoming[{id, name, family, current, target, description}], latestBadge}`

**GET /learner/subjects** → `{subjects: [{id, name, weight, topicCount, topicsStarted, topicsMastered, questionsAnswered, averageMastery}]}`

**GET /learner/profile** → `{profile: {displayName, email, timezone, createdAt, preferredLanguage}}`.
Returns 404 `PROFILE_NOT_FOUND`.

**PATCH /learner/profile** → `{profile}`. Send at least one field:

| Field | Rule |
|---|---|
| `displayName` | 1–60 |
| `timezone` | IANA name the server can load |
| `preferredLanguage` | `english` \| `hindi` \| `hinglish` |

**GET /learner/confidence** → `{ratings: [{topicId, confidence}]}`

**PUT /learner/confidence** `{ratings: [{topicId, confidence 1–5}]}` → `{ratings}`.
1–50 ratings with unique topic ids. Unknown fields are rejected.

## Plan, readiness, revision

**GET /study-plan/current** → `{plan: Plan | null, adjustment: {reason, explanation} | null}`.
Opening the plan first settles past sessions and may rebuild the plan (`missed_sessions` or
`poor_performance`; see [learner-model.md](learner-model.md#study-plan-planningbuildplan)).

`Plan` has these fields:

- `id`, `version`, `examDate`, `dailyMinutes`, `daysRemaining`
- `totalMinutesPlanned`, `topicsCovered`, `reason`, `createdAt`
- `days[{date, totalMinutes, sessions[{id, topicId, topicName, plannedMinutes, plannedQuestions, kind, reason, status}]}]`
- `omittedTopics[{name, reason}]` (only on a fresh build)

**POST /study-plan/generate** (no body) → 201 `{plan}`. All errors are 400:

| Code | Meaning |
|---|---|
| `NO_ACTIVE_GOAL` | No goal is set |
| `EXAM_DATE_PASSED` | The exam date has passed |
| `NOTHING_TO_PLAN` | No topics in scope |
| `PLAN_EMPTY` | Not enough time for even one session |

**GET /readiness** returns one of:

- `{goal: null, readiness: null}`
- `{goal{title, examDate, daysRemaining, dailyMinutes}, readiness, thresholds{ready: 75, onTrack: 55, building: 30, masteryTarget: 85}}`

`readiness` has these fields:

- `score`, `band`, `confidence`, `verdict`
- `components[{key, label, score, weight, detail}]`
- `gaps[{topicId, name, subjectName, masteryScore, cost}]`, `notStarted[…]`
- `daysRemaining`, `topicsInScope`, `topicsAttempted`, `topicsMastered`, `mocksTaken`

**GET /revision/due** → `{today, due: [{topicId, name, subjectName, dueOn, intervalDays, lapses, reviewCount, lastReviewAccuracy, masteryScore, daysOverdue}]}`.
At most 20, longest overdue first.

**GET /revision/flashcards** → `{today, topics: [{topicId, name, reason: "due" | "weak", dueOn, masteryScore, cards: [{id, front, back, sourceChunkId, citations: [Citation]}]}], incomplete}`.

- Covers up to 4 topics: due ones, then those with mastery under 60, and only topics that have the learner's own excerpts.
- Missing decks are written by the AI on first request, 6 cards per topic.

**POST /revision/flashcards/{id}/review** `{known: bool}` → `{topicId, moved, schedule: ScheduleUpdate | null}`.
Returns 404 `FLASHCARD_NOT_FOUND`.

`ScheduleUpdate` = `{dueOn, intervalDays, easeFactor, reviewCount, lapses, lastReviewedOn, lastReviewAccuracy, lapsed, finishedForThisGoal}`.

## Groups

`GroupSummary` = `{id, name, inviteCode, memberCount, role, createdAt}`.

| Route | Body | Success | Errors |
|---|---|---|---|
| GET `/groups` | — | `{groups: [GroupSummary]}` | |
| POST `/groups` | `{name: 2–60}` | 201 `{group}` | 409 `TOO_MANY_GROUPS` (limit 10), 409 `GROUP_FULL` |
| POST `/groups/join` | `{inviteCode}`, 6 characters after normalising | `{group}`. Joining twice is not an error. | 400 `INVALID_CODE`, 404 `GROUP_NOT_FOUND`, 409 `TOO_MANY_GROUPS`, 409 `GROUP_FULL` |
| GET `/groups/{id}` | — | `{group: {…GroupSummary, comparison{members[{userId, displayName, questionsAnswered, currentStreakDays, topicsMastered, isYou, rank}], yourRank, memberCount, totals{questionsAnswered, topicsMastered}, topStreak{displayName, days}}}}` | 404 `GROUP_NOT_FOUND` (also when not a member) |
| POST `/groups/{id}/leave` | — | `{left: true}` | 404 `GROUP_NOT_FOUND` |

## Practice and assessment

**POST /attempts** → 201

Body:

| Field | Rule |
|---|---|
| `questionId` | uuid |
| exactly one answer field | `selectedOptions` (int[] ≤ 20 items, each 0–50), `value` (number) or `text` (≤ 2000) |
| `timeTakenSeconds` | 0–3600, optional |
| `source` | `practice` (default), `assessment`, `topic_test`, `mock_test` or `revision` |

Response:

- `attemptId`, `isCorrect`
- `correctAnswer`: the key, released only after the answer is recorded
- `explanation`, `sourceChunkId`
- `feedback`: from the rubric grader, short answers only
- `misconception`
- `mastery{topicId, before, after, change, attemptsOnTopic}`

Errors:

| Status | Code | When |
|---|---|---|
| 404 | `QUESTION_NOT_FOUND` | Missing or unverified question |
| 400 | `INVALID_ANSWER` | The answer kind does not match the question type |
| 500 | `QUESTION_MALFORMED` | The stored question cannot be graded |
| AI errors | | A short answer that matches no accepted wording goes to the rubric grader |

**GET /recommendations/next?minutes=1–600&subject=uuid** → 200, in one of three shapes:

| Case | `data` |
|---|---|
| Normal | `{recommendation, questions, bankNote}` |
| Nothing in scope | `{recommendation: null, questions: [], reason}` |
| No questions available | `{recommendation: null, questions: [], reason}` |

- `recommendation` = `{action: learn_new | practice | revise | mini_test, topic{id, name}, difficulty, questionCount, estimatedMinutes, reason, signals{masteryScore, recentAccuracyPercent, totalAttempts, daysSinceLastAttempt}, alternatives[{topicId, name, score, masteryScore}]}`.
- `questions[]` = `{id, topic_id, question_type, body, options, difficulty, marks}`, with no answer.
- An empty bank is filled synchronously with up to 3 verified questions (Redis lock `qgen:<topicId>`).

**POST /questions/generate** `{topicId, difficulty?: easy | medium | hard (default medium), count?: 1–5 (default 3)}` → 201

- Response: `{topic, requested, outcomes[{body, difficulty, verified, reason, stored}], discarded[{reason}], usage{inputTokens, outputTokens, calls}}`.
- Errors: 503 `AI_UNAVAILABLE`, 404 `TOPIC_NOT_FOUND`, AI errors.
- See [grounding.md](grounding.md#generated-questions-source-tagged-and-independently-verified).

**GET /assessments/diagnostic** → `{assessment: Assessment | null, limits{min: 6, max: 25, default: 15}}`.
Returns 400 `NO_ACTIVE_GOAL`.

`Assessment` has these fields:

- `id`, `status`, `questionCount`, `durationMinutes`, `startedAt`, `completedAt`
- `questions[]`: while open
- `result{correctCount, answeredCount, totalQuestions, accuracyPercent, subjects[{subjectId, subjectName, correct, total, percent, level}], verdict}`: once completed
- `coverage[{subjectName, questionCount}]`, `uncovered[{subjectName, reason}]`: on start only

**POST /assessments/diagnostic** `{questionCount?: 6–25}` → 201 `{assessment}`. Abandons any open diagnostic first.
Errors: 400 `NO_ACTIVE_GOAL`, 400 `NOTHING_TO_ASSESS`.

**GET /assessments/{id}** → `{assessment}`. Returns 404 `ASSESSMENT_NOT_FOUND`.

**POST /assessments/{id}/submit** `{answers: [{questionId, selectedOptions? | value? | text?}] (≤ 25)}` → `{assessment}` with its `result`.

- Idempotent: an assessment that is already completed is not re-marked.
- An answer with no answer field counts as unanswered.
- Returns 404 `ASSESSMENT_NOT_FOUND`.

**GET /mock-tests** → `{tests[MockTest] (latest 10), open: MockTest | null, topics[{topicId, name, subjectName, weight, masteryScore, plannedSessions, availableQuestions}], limits{min: 5, max: 30, default: 15}}`

**POST /mock-tests** `{questionCount?: 5–30, topicIds?: 1–30 uuids from the goal}` → 201 `{test}`

- Source is `chosen` when `topicIds` is sent, otherwise `plan`, otherwise `goal`.
- Abandons any open test first.
- Returns 400 `NOTHING_TO_TEST`.

**GET /mock-tests/{id}** → `{test}`. Returns 404 `TEST_NOT_FOUND`.

`MockTest` has these fields:

- `id`, `title`, `source`, `status`, `totalQuestions`, `durationMinutes`, `startedAt`, `submittedAt`, `correctCount`, `scorePercent`
- `questions`: while open
- `result{correctCount, answeredCount, totalQuestions, scorePercent, passed, byTopic[{topicId, name, correct, total, percent}], verdict, misconceptions}`: once submitted
- `review[{questionId, topicId, topicName, questionType, body, options, status: correct | wrong | skipped | unmarked, givenAnswer, correctAnswer, explanation, feedback, misconception, source: Citation | null}]`: once submitted

**POST /mock-tests/{id}/submit** `{answers (≤ 30), secondsTaken: 0–86400}` → `{test}` (submitted).
Idempotent. Returns 404 `TEST_NOT_FOUND`.

**GET /misconceptions** → `{topics: [{topicId, topicName, masteryScore, misconceptions: [{label, occurrences, lastSeenAt, source: Citation | null}]}]}`.
Weakest topic first, up to 3 misconceptions per topic.

## Course material

`Source` = `{id, kind: pdf | slides | video, title, status: processing | ready | failed, error, excerptCount, curriculumId, createdAt}`.

**GET /sources** → `{sources: [Source], organize: {status: idle | waiting | failed, message?}}`

**POST /sources/upload?kind=pdf|slides&title=…** with the raw file as the body (not multipart), up to 40 MB → 202 `{source: {id, status: "processing"}}`

| Status | Code | When |
|---|---|---|
| 413 | `PAYLOAD_TOO_LARGE` | The file is over the limit |
| 400 | `UNSUPPORTED_FILE` | `pdf` needs `%PDF`; `slides` accepts `.pptx` or PDF |
| 400 | `INVALID_INPUT` | Empty body, or a bad kind or title |
| 503 | `STORAGE_UNAVAILABLE` | Storage is not configured |

**POST /sources/video** `{url: YouTube link, title: 1–200}` → 202 `{source}`

**POST /sources/video/captions?url=…&title=…** with the raw `.vtt`/`.srt` text as the body, up to 5 MB → 202 `{source}`.
Returns 400 if the body has no `-->`.

**POST /sources/organize** `{courseName: 2–120, whenReady?: bool}` returns one of:

- 201 `CourseMap`.
- 202 `{status: "waiting"}`, when `whenReady` is set and sources are still processing.

Errors: 409 `NO_MATERIAL`, 502 `AI_MALFORMED`, AI errors.

`CourseMap` = `{curriculum: {id, name} | null, subjects: [{id, name, topics: [{id, name, keyConcepts, excerptCount, prerequisites: [{id, name}]}]}]}`.

**GET /sources/map/{curriculumId}** → `CourseMap`

**GET /sources/excerpts/{id}/open** → `{url, slide, sourceTitle, location, excerpt}`.
Errors: 404 `EXCERPT_NOT_FOUND`, 404 `SOURCE_NOT_FOUND`.

`url` depends on the source:

- a video: `…watch?v=ID&t=Ns`;
- a PDF: a signed link with `#page=N`;
- a deck: a signed link;
- otherwise: null.

**DELETE /sources/{id}** → `{deleted: true}`. Also deletes its excerpts and the stored file.
Returns 404 `SOURCE_NOT_FOUND`.

## Tutor

`Citation` = `{n, chunkId, sourceId, sourceTitle, kind, modality: text | figure | transcript, location, excerpt, topicId?}`.

**GET /ai/conversations** → `{conversations: [{id, title, topicId, lastMessageAt, answered}]}`. Latest 30.

**GET /ai/conversations/{id}** → `{id, title, topicId, messages: [{role, content, createdAt, citations}]}`.
Latest 50 messages. Returns 404 `CONVERSATION_NOT_FOUND`.

**POST /ai/tutor** → 201

Body:

| Field | Rule |
|---|---|
| `question` | 2–2000 |
| `conversationId` | uuid, optional |
| `topicId` | uuid, optional |
| `currentQuestion` | ≤ 2000, optional. The practice question the learner is looking at. |

Response:

- `conversationId`, `title`, `answer`
- `grounding`: `source`, `partial`, `none`, or null when the learner has no material
- `citations[Citation]`
- `messages[2]`
- `usage{inputTokens, outputTokens, model}`
- `topicId`
- `check: {questionId, body, options, citation} | null`

Errors: 404 `CONVERSATION_NOT_FOUND`, AI errors.

**POST /ai/checks/{id}/answer** `{selectedOption: 0–9}` → 201 `{attemptId, isCorrect, correctAnswer, explanation, sourceChunkId, mastery{topicId, before, after}}`.
Recorded as an attempt with source `tutor`. Returns 404 `QUESTION_NOT_FOUND`.
