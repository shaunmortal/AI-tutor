# Security

**In short:** the learner's identity comes from a verified token. The database enforces who may
read what, through RLS and column grants, even for the API's own reads. Learners cannot write
anything they are judged by. Every expensive route has a per-learner quota in Redis.

## 1. Identity comes from a verified token only

`internal/auth`:

- `Require` takes `Authorization: Bearer <token>` and rejects it unless it is 20–4096 characters.
- It verifies the token with Supabase (`GET /auth/v1/user`, using the publishable key).
- The verified user id is cached in Redis under `auth:<sha256(token)>` for **1 minute**, so a revoked token keeps working for at most a minute.
- Handlers read the id with `auth.UserID(r)`. That call panics if the middleware is missing, so no route can run unauthenticated by accident.
- A `user_id` in a body, query string or header is never used.

Every route except `/api/v1/health` and `/api/v1/health/deep` is mounted through `app.Deps.Authed`,
which always wraps it in `Require`.

## 2. Row-level security, enforced on API reads too

- Every table has RLS enabled. Learner-owned tables allow `SELECT` only where `auth.uid() = user_id`.
- The API reads a learner's own data through `DB.AsUser`. It runs each transaction with `role = authenticated` and the learner's JWT claims set locally, so a query that forgets its `WHERE` clause still cannot return another learner's rows.
- `DB.Admin` bypasses RLS and is used only where it is needed: grading against the answer key, writes to tables learners cannot write, and group comparisons. Every such query filters by the token's user id. See [architecture.md](architecture.md#admin-and-asuser-keeping-rls-in-force).

## 3. The answer key never reaches the browser early

`questions.correct_answer`, `explanation` and `option_misconceptions` are **not granted** to
`authenticated`. That is a column grant, not a convention. Only `is_verified` rows are visible.
Grading runs on the server, and the key is returned only:

- in the response to a recorded answer; or
- in the review of a submitted mock test.

Mock test feedback and misconceptions are written only at marking time. Details are in
[database.md](database.md#the-answer-key-column-grant).

## 4. Learners cannot write what they are judged by

- `authenticated` holds `SELECT` only on goals, attempts, mastery, plans, sessions, revision schedule, assessments, mock tests, self-ratings, mistake patterns, flashcards, sources and excerpts.
- The only learner-writable columns are `profiles.display_name`, `avatar_url`, `timezone`, `settings` and `preferred_language`, on their own row.
- All other writes go through the API, after validation, on `DB.Admin`.
- Migration 024 revokes Supabase's default `TRUNCATE`/`REFERENCES`/`TRIGGER` grants from `anon` and `authenticated`, because `TRUNCATE` ignores RLS. `anon` has no table privileges at all.
- The database also refuses impossible data through `CHECK` constraints: mastery 0–100, ease 1.3–3.0, an excerpt without a location, a short question without a key, and so on.

## 5. Course material and AI input

- **Uploads.**
  - The file type is decided by magic bytes, not by the filename.
  - Size is capped at 40 MB for files and 5 MB for captions (413 above that).
  - Files are stored in the **private** bucket `course-sources` under `<userId>/<random>`. They are only ever opened through signed URLs that expire after **1 hour**, and only after the API has checked that the excerpt belongs to the caller.
- **Search** functions filter by learner id. Execute is revoked from browser roles.
- **Prompt injection.**
  - Excerpts, typed answers and course text are passed to the model as data.
  - The grounded tutor, course map, sourced question and rubric grader prompts each state that this content is not instructions.
  - `CheckGrounding` removes citations to excerpts that were never provided, and moves uncited claims out of the "from your course" section.
- **Generated questions** are only shown to learners after an independent second solve agrees with them (`is_verified`). The exception is the tutor's quick checks, which only the learner who received them can answer.

## 6. Keys: what is secret and what is public

| Value | Where | Secret? |
|---|---|---|
| `SUPABASE_SECRET_KEY` | API only (Storage) | **Yes** |
| `DATABASE_URL` (owner credentials) | API only | **Yes** |
| `ANTHROPIC_API_KEY`, `GEMINI_API_KEY` | API only | **Yes** |
| `REDIS_URL` | API only | **Yes** |
| `SUPABASE_URL`, `SUPABASE_PUBLISHABLE_KEY` | API and browser (`VITE_…`) | No. Public by design; every table is behind RLS and grants. |
| `VITE_API_URL` | Browser | No |

- `server/.env` is gitignored. In production, secrets are set in the host dashboard; `render.yaml` marks them `sync: false`, so they are never in the repository.
- Only `VITE_`-prefixed variables reach the browser bundle.

## 7. Abuse limits

**Per-learner hourly quotas** (`internal/ratelimit`): fixed windows in Redis, keyed on the verified
user id and never on an IP address. They are shared across instances and survive restarts. If Redis
is down they fail open, so studying is never blocked.

| Quota | Limit / hour | Routes |
|---|---|---|
| `ai-tutor` | 40 | `POST /ai/tutor` |
| `questions-generate` | 10 | `POST /questions/generate` |
| `sources-ingest` | 30 | `POST /sources/upload`, `/sources/video`, `/sources/video/captions` |
| `sources-organize` | 10 | `POST /sources/organize` |
| `curricula-resolve` | 20 | `POST /curricula/resolve` |
| `plan-generate` | 20 | `POST /study-plan/generate` |
| `assessments-start` | 10 | `POST /assessments/diagnostic` |
| `mock-tests-generate` | 20 | `POST /mock-tests` |
| `groups-join` | 30 | `POST /groups/join` (guessing invite codes) |

**Request limits:**

- JSON bodies are capped at 100 KiB.
- Read and write timeouts are 2 and 3 minutes.
- Headers are capped at 64 KiB.
- Each query has a 15 s `statement_timeout`.
- Each instance sheds load past `MAX_IN_FLIGHT` with `503 SERVER_BUSY` (see [architecture.md](architecture.md#overload-shedding)).

## 8. Not found rather than forbidden

A resource the caller does not own answers **404**, not 403. A malformed id gets the same 404 on
the group and test routes, so probing cannot confirm that an id exists.

| Request | Response |
|---|---|
| `GET /groups/{not mine}` | 404 `GROUP_NOT_FOUND` |
| `GET /mock-tests/{not mine}` | 404 `TEST_NOT_FOUND` |
| `GET /assessments/{not mine}` | 404 `ASSESSMENT_NOT_FOUND` |
| `GET /ai/conversations/{not mine}` | 404 `CONVERSATION_NOT_FOUND` |
| `GET /sources/excerpts/{not mine}/open` | 404 `EXCERPT_NOT_FOUND` |

## 9. Study groups

- Members see each other's display name, questions answered, current streak and topics mastered. They see nothing else: no answers, no mastery per topic, no goals.
- Group reads are gated by `is_group_member`.
- Invite codes are 6 characters from an alphabet without look-alike characters (`^[A-HJ-NP-Z2-9]{6}$`).
- A group holds at most 30 members, enforced by a trigger. A learner can be in at most 10 groups.

## 10. CORS

The API echoes `Access-Control-Allow-Origin` only for origins listed in `CORS_ORIGINS`
(comma-separated, exact match). It allows the headers `Authorization, Content-Type` and the methods
`GET, POST, PUT, PATCH, DELETE, OPTIONS`.

## Known limitations

- **Token checks call Supabase.** One verification per token per minute; a revoked token works for up to a minute. Verifying signatures locally against the project's JWKS would remove that hop.
- **Question verification is a second model call, not a human.** A question that both calls get wrong in the same way would still reach learners.
- **Invite codes cannot be rotated.** The 30-member cap limits the damage.
- **No audit log** of elevated writes.
- **The curriculum named in `GET /sources/map/{curriculumId}` is not checked for ownership.** Only the excerpt counts are filtered by learner. Topic names in a course map are therefore readable by id, in the same way that `topics` is readable by any signed-in learner.

## Checking it yourself

```sql
-- correct_answer must not be in the learner's column grant
select column_name from information_schema.column_privileges
 where table_name = 'questions' and grantee = 'authenticated';

-- RLS is on for every table
select relname from pg_class c join pg_namespace n on n.oid = c.relnamespace
 where n.nspname = 'public' and c.relkind = 'r' and not c.relrowsecurity;   -- expect no rows

-- learners hold no table-level write privilege
select table_name, privilege_type from information_schema.role_table_grants
 where grantee in ('authenticated', 'anon') and privilege_type <> 'SELECT';  -- expect no rows
```

The Go integration tests (`go test ./...` with `DATABASE_URL` set) cover the RLS cases as a
learner: another learner's goals and attempts are invisible, learners cannot write goals, and
`correct_answer` cannot be read.
