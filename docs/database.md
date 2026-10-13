# Database

Postgres on Supabase. The schema lives in `database/migrations`, applied in filename order. SQL
tests are in `database/tests`, and demo data is in `database/seed`.

```bash
for f in database/migrations/*.sql; do psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -1 -f "$f"; done
psql "$DATABASE_URL" -f database/seed/demo_questions.sql
```

## Principles

- **RLS on every table.** Each table enables row-level security in its own migration. Learner-owned rows use `using (auth.uid() = user_id)`. Shared reference data (`topics`, `curricula`, `topic_prerequisites`) is readable by any signed-in learner.
- **Nothing is exposed by default.** The project has "automatically expose new tables" turned off (`000_roles.sql`). `anon` is granted nothing. `authenticated` is granted table by table, and sometimes column by column, in each table's migration. `service_role` gets everything, and that role never leaves the server.
- **Learners write almost nothing.** `authenticated` holds `SELECT` only on almost every table. The only learner writes are `UPDATE (display_name, avatar_url, timezone, settings, preferred_language)` on their own `profiles` row. Attempts, mastery, goals, plans, self-ratings, misconceptions and flashcards are written by the API through `DB.Admin`, after validation.
- **The answer key is a column learners are not granted** (see below).
- **Default grants that bypass RLS are revoked.** `024_revoke_default_grants.sql` removes `TRUNCATE`, `REFERENCES` and `TRIGGER` from `anon` and `authenticated` on all tables, and from default privileges. `TRUNCATE` ignores RLS.
- **Search functions are API-only.** Execute on `search_source_chunks` and `hybrid_search_source_chunks` is revoked from `public`, `anon` and `authenticated`.
- **Constraints over app code.** Enums, ranges, shapes and "one active X per learner" are all `CHECK` constraints or unique indexes.

The API reads a learner's own rows through `DB.AsUser`, which sets `role = authenticated` and the
JWT claims for the transaction. RLS therefore applies to API reads as well as to the browser (see
[architecture.md](architecture.md#admin-and-asuser-keeping-rls-in-force)).

## The answer-key column grant

```sql
-- 003_questions.sql
create policy "Learners read verified questions" on public.questions
    for select to authenticated using (is_verified);

grant select (id, topic_id, question_type, body, options, difficulty, marks)
    on public.questions to authenticated;

-- 023_course_sources.sql
grant select (source_chunk_id) on public.questions to authenticated;
```

`correct_answer`, `explanation` and `option_misconceptions` are not in the grant. A learner who
selects them gets a permission error, not an empty column. Grading runs on the server (`DB.Admin`),
and the key is returned only in the response to a recorded answer, or in a submitted test's review.
The key covers every question type: option indexes, `{value, tol}` for numeric questions, and
`{answers, rubric}` for short answers. `option_misconceptions` is withheld because its empty
entries would reveal which options are correct.

## Tables

| Table | Migration | Holds | Key constraints / indexes | Learner access |
|---|---|---|---|---|
| `profiles` | 001, 028 | One row per auth user: display name, timezone, settings, `preferred_language` | `preferred_language` ∈ english/hindi/hinglish | Read own. Update five columns. |
| `topics` | 002, 023 | Syllabus tree (subject → topic), `weight` 0–5, `key_concepts` | Unique `(parent_id, name)` with nulls not distinct; no self-parent | Read all |
| `questions` | 003, 023, 025, 026 | Question bank | Type ∈ mcq/msq/numeric/short; difficulty ∈ easy/medium/hard; marks 1–100; options match type; a short question must have `answers[]` and `rubric`; `option_misconceptions` aligned with `options`; unique `(topic_id, md5(body))` | Verified rows only; seven columns plus `source_chunk_id` |
| `learning_goals`, `learning_goal_topics` | 004 | Exam date, `daily_minutes` 10–960, subjects in scope | Partial unique index: one `active` goal per learner | Read own |
| `question_attempts` | 005, 027 | The append-only answer log | `source` ∈ practice/assessment/topic_test/mock_test/revision/tutor; time 0–3600 s; unique `(user_id, question_id, attempt_number)` | Read own |
| `assessments`, `assessment_questions` | 006 | Diagnostic papers | kind ∈ diagnostic/checkpoint; status ∈ in_progress/completed/abandoned; `correct_count ≤ question_count`; unique position | Read own |
| `learner_profiles` | 007 | Learner-level cache: totals, accuracy, consistency, streaks, pace, preferred difficulty | Counts ≥ 0, correct ≤ total, consistency 0–100 | Read own |
| `concept_mastery` | 008 | Per-topic mastery cache and the evidence behind it | PK `(user_id, topic_id)`; score 0–100; difficulty buckets add up to the total; streak ≤ correct | Read own |
| `curricula` | 009, 031 | A syllabus learners can prepare for (seeded, AI-resolved, or built from material). `is_course_map` marks one built from a learner's own material. | `slug` unique and shaped `^[a-z0-9][a-z0-9-]{1,79}$` | Read all (the API's `GET /sources/map` serves a course map only to its learner) |
| `study_plans` | 011 | Versioned plans | status ∈ active/superseded/completed/abandoned; reason ∈ initial/missed_sessions/poor_performance/goal_changed/requested; unique `(goal_id, version)`; one active plan per learner | Read own |
| `study_sessions` | 012 | Day-by-day sessions in a plan | kind ∈ learn/revise/assess; status ∈ pending/completed/partial/missed/skipped; unique `(plan_id, scheduled_date, topic_id)` | Read own |
| `revision_schedule` | 013 | SM-2 state per learner and topic | PK `(user_id, topic_id)`; interval 1–365; ease 1.30–3.00; lapses ≤ reviews | Read own |
| `ai_conversations`, `ai_messages` | 015, 023 | Tutor threads; messages keep `citations` (jsonb), tokens and model | role ∈ learner/tutor; content 1–20000 characters; token fields only on tutor messages | Read own |
| `mock_tests`, `mock_test_questions` | 017, 025, 026 | Mock tests and per-question marking (`given_answer`, `is_correct`, `feedback`, `misconception`) | status ∈ in_progress/submitted/abandoned; source ∈ plan/goal/chosen; 1–40 questions; 1–300 minutes | Read own |
| `study_groups`, `group_members` | 022 | Groups with a six-character invite code | Code shape `^[A-HJ-NP-Z2-9]{6}$`; role ∈ owner/member | Members read their groups (`is_group_member`) |
| `course_sources` | 023 | Uploaded PDFs and decks, and video links | kind ∈ pdf/slides/video; status ∈ processing/ready/failed; must have `storage_path` or `video_url` | Read own |
| `source_chunks` | 023, 029 | Located excerpts: `modality`, `page`/`slide`/`start_sec`/`end_sec`, `content`, generated `tsv`, `embedding vector(768)` | modality ∈ text/figure/transcript; **at least one location is required**; GIN on `tsv`, HNSW (cosine) on `embedding` | Read own; not `user_id`, `tsv` or `embedding` |
| `topic_prerequisites` | 023 | Prerequisite edges from the course map | PK `(topic_id, prerequisite_id)`; no self-edges | Read all |
| `mistake_patterns` | 026 | Misconceptions per learner and topic, with occurrence counts and evidence attempt ids | Unique `(user_id, topic_id, lower(misconception))`; label 1–300 characters | Read own |
| `flashcards` | 027 | Revision cards, each tied to a `source_chunk_id` | front 1–500, back 1–1000 characters | Read own |
| `self_ratings` | 028 | Onboarding confidence (1–5) per subject or topic | PK `(user_id, topic_id)`; confidence 1–5 | Read own |

Storage: the private bucket `course-sources` (created in 023). Files are reached only through
signed links that the API issues, valid for 1 hour.

## Migrations

There are 32 files from `000` to `031`. Seven are empty placeholders, kept so the numbering stays
stable: `009_mistake_patterns`, `010`, `014`, `016`, `019`, `020`, `021`. Misconceptions arrived in
026. Number 018 is unused.

| File | Adds |
|---|---|
| `000_roles.sql` | Schema usage; all privileges to `service_role` (current and default); nothing to `anon` |
| `001_profiles.sql` | `profiles`, created on signup |
| `002_topics.sql` | `topics` tree |
| `003_questions.sql` | `questions`, verified-only policy, column grant without the answer key |
| `004_learning_goals.sql` | `learning_goals`, `learning_goal_topics` |
| `005_question_attempts.sql` | `question_attempts` |
| `006_assessments.sql` | `assessments`, `assessment_questions` |
| `007_learner_profiles.sql` | `learner_profiles` |
| `008_concept_mastery.sql` | `concept_mastery` |
| `009_curricula.sql` | `curricula` |
| `011_study_plans.sql` | `study_plans` |
| `012_study_sessions.sql` | `study_sessions` |
| `013_revision_schedule.sql` | `revision_schedule` |
| `015_ai_conversations.sql` | `ai_conversations`, `ai_messages` |
| `017_mock_tests.sql` | `mock_tests`, `mock_test_questions` |
| `022_study_groups.sql` | `study_groups`, `group_members`, `is_group_member` |
| `023_course_sources.sql` | `course_sources`, `source_chunks`, `topic_prerequisites`, `topics.key_concepts`, `questions.source_chunk_id`, `ai_messages.citations`, `search_source_chunks`, the Storage bucket |
| `024_revoke_default_grants.sql` | Revokes `TRUNCATE`/`REFERENCES`/`TRIGGER` from `anon`/`authenticated` |
| `025_short_answer_questions.sql` | `short` question type and its key check; mock test source `chosen`; `mock_test_questions.feedback` |
| `026_mistake_patterns.sql` | `questions.option_misconceptions`, `mistake_patterns`, `mock_test_questions.misconception` |
| `027_tutor_checks_and_flashcards.sql` | Attempt source `tutor`; `flashcards` |
| `028_onboarding.sql` | `profiles.preferred_language`; `self_ratings` |
| `029_chunk_embeddings.sql` | pgvector, `source_chunks.embedding vector(768)`, HNSW index, `hybrid_search_source_chunks` (RRF, k = 60) |
| `030_hybrid_similarity_floor.sql` | `hybrid_search_source_chunks`: vector matches need cosine similarity ≥ 0.67 |
| `031_course_map_curricula.sql` | `curricula.is_course_map`, backfilled for existing course maps |

`server/cmd/backfill-embeddings` embeds excerpts that were stored before 029, or whose embedding
call failed.
