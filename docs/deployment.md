# Deployment

This deploys:

- the Go API and its Redis on Render, using `render.yaml`;
- the web app on Vercel;
- Postgres, Auth and Storage on Supabase.

Any host that runs a Go binary next to a Redis works the same way. For several instances behind a
balancer, see [architecture.md](architecture.md#stateless-scaling-and-the-load-balancer).

Placeholders used below:

- `https://<your-api>.onrender.com`: your API host
- `https://<your-site>.vercel.app`: your web app

## 1. Supabase

1. Create a project. Pick a region close to where the API will run; every query pays that round trip.
2. **Apply the schema** in filename order, then load the starter question bank:
   ```bash
   for f in database/migrations/*.sql; do psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -1 -f "$f"; done
   psql "$DATABASE_URL" -f database/seed/demo_questions.sql
   ```
   Migration 029 creates the `vector` extension in the `extensions` schema.
3. **Data API:** keep "automatically expose new tables" **off**. The migrations grant access table by table (`000_roles.sql`).
4. **Authentication:**
   - Turn **Confirm email** off for demos. With it on, sign-up returns no session, and the built-in mailer allows only a few emails an hour.
   - Under URL configuration, set the Site URL to `https://<your-site>.vercel.app`, and add `https://<your-site>.vercel.app/**` and `http://localhost:5173/**` to the redirect URLs. Password reset and social sign-in need them.
   - Google, Facebook and LinkedIn sign-in each have to be enabled in that provider's console and in Supabase. The sign-in screen detects a disabled provider and says so.
5. **Keys** (Project Settings → API Keys and Connect):

| Value | Use |
|---|---|
| Project URL | `SUPABASE_URL`, `VITE_SUPABASE_URL` |
| Publishable key | `SUPABASE_PUBLISHABLE_KEY`, `VITE_SUPABASE_PUBLISHABLE_KEY` |
| Secret key | `SUPABASE_SECRET_KEY` (API only: Storage) |
| Connection string | `DATABASE_URL`. Use the **transaction pooler** (`:6543`) with `DB_POOL_MODE=transaction` when several instances share the database, or the session pooler (`:5432`) for one instance. Percent-encode `@ # / : %` in the password. |

The `course-sources` Storage bucket is created by migration 023 as private.

## 2. API and Redis on Render

Create a **Blueprint** from the repository. `render.yaml` defines two services:

| Service | Settings |
|---|---|
| `adigam-api` (web, Go) | `rootDir: server`. Build: `go build -trimpath -ldflags="-s -w" -o api ./cmd/api`. Start: `./api`. `PORT=10000`, `DB_POOL_MODE=transaction`, and `REDIS_URL` taken from the Redis service. |
| `adigam-redis` (key-value) | `maxmemoryPolicy: noeviction`, because queued ingestion jobs must never be evicted. `ipAllowList: []`, so it is reachable only inside Render's private network. |

Render asks once for every variable marked `sync: false` and stores it encrypted:

| Variable | Value |
|---|---|
| `CORS_ORIGINS` | `https://<your-site>.vercel.app`: the web app's origin, with no trailing slash. Comma-separate further origins. |
| `SUPABASE_URL`, `SUPABASE_PUBLISHABLE_KEY`, `SUPABASE_SECRET_KEY` | From Supabase |
| `DATABASE_URL` | Transaction-pooler connection string |
| `ANTHROPIC_API_KEY` | Claude key. The API runs without one, but AI routes then answer 503. |

Variables that are not in the blueprint can be added in the dashboard when needed:

| Variable | When |
|---|---|
| `AI_PROVIDER=gemini` with `GEMINI_API_KEY` | To use Gemini instead of Claude |
| `GEMINI_API_KEY` alone | Enables embeddings and hybrid retrieval with either provider. Without it, search is full-text only. |
| `AI_MODEL_HIGH`, `AI_MODEL_STANDARD` | To change models per task |
| `MAX_IN_FLIGHT` | Per-instance concurrency before shedding. Default 256. |

The API refuses to start without `SUPABASE_URL`, `SUPABASE_PUBLISHABLE_KEY` and `DATABASE_URL`, or
when `AI_PROVIDER` is anything other than `claude` or `gemini`.

The platform health check is left off on the free plan: with one instance, a single slow check takes
the service out of rotation. Point monitors at `/api/v1/health/deep`, which also checks the
database and Redis.

Verify:

```bash
curl -s https://<your-api>.onrender.com/api/v1/health/deep
# {"success":true,"data":{"status":"ok","database":"reachable","redis":true,…},"error":null}
```

## 3. Web app on Vercel

Import the repository with **Root directory** set to `frontend`. Vercel detects Vite, which builds
with `tsc -b && vite build`. Set the environment variables:

| Variable | Value |
|---|---|
| `VITE_API_URL` | `https://<your-api>.onrender.com/api/v1` |
| `VITE_SUPABASE_URL` | Supabase project URL |
| `VITE_SUPABASE_PUBLISHABLE_KEY` | Publishable key |

- These three are bundled into the browser and are public by design.
- `vite.config.ts` fails the build if any of them is missing. Without that check, the build would succeed and the app would show a blank screen on load.
- `frontend/vercel.json` rewrites every path to `index.html` (SPA routing). It serves `/assets/*` as immutable for a year and `index.html` with no caching.

After the first deploy, set `CORS_ORIGINS` on the API to the exact site origin.

## 4. Keep-alive

`.github/workflows/keep-alive.yml` runs every 10 minutes, and can also be started by hand. It
`curl`s the deep health check, which keeps the free web instance from spinning down and keeps the
free database from pausing after a week idle. It tries up to 5 times with a widening gap (cold
starts are expected) and fails the run if no 200 comes back.

It reads the **repository variable** `API_HEALTH_URL` (Settings → Secrets and variables → Actions →
Variables), set to `https://<your-api>.onrender.com/api/v1/health/deep`. While the variable is
unset, the job logs "API_HEALTH_URL is not set; skipping." and succeeds.

Running a service awake around the clock uses about 730 of the free plan's 750 instance-hours a
month, which is fine for one service on the account and not for two.

## 5. CI

`.github/workflows/ci.yml` runs on every push and pull request to `main`:

- **Go:** `gofmt` check, `go vet`, `go test -race ./...` against a real Redis service, and builds for Linux and Windows.
- **Frontend:** lint, format check, tests, and a build with placeholder `VITE_` values.

Database integration tests skip without `DATABASE_URL`; run them by hand against the project.
`database/tests` is also run by hand, and each file rolls back.

## 6. Verify the deployment

```bash
API=https://<your-api>.onrender.com/api/v1
SITE=https://<your-site>.vercel.app

curl -s $API/health/deep                                                   # database reachable, redis true
curl -s -o /dev/null -w '%{http_code}\n' $API/learner/summary              # 401: auth is enforced
curl -s -D- -o /dev/null -X OPTIONS $API/learner/summary \
  -H "Origin: $SITE" -H "Access-Control-Request-Method: GET" | grep -i access-control-allow-origin   # echoes $SITE
curl -s -o /dev/null -w '%{http_code}\n' $SITE/dashboard                   # 200: SPA rewrite works
```

Then, in a browser:

1. Sign up.
2. Add a PDF on Sources and wait for it to become *ready*.
3. Ask the tutor a question about it, then tap a citation. It should open the right page.
4. Answer a practice question, and check that the mastery number moves.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Browser shows CORS errors | `CORS_ORIGINS` does not exactly match the site origin (scheme, host, no trailing slash) |
| Every request is 401 | `SUPABASE_URL` or publishable key mismatch between the API and the web app |
| `EMAXCONNSESSION` or connection errors under load | Session pooler with several instances. Switch to `:6543` and `DB_POOL_MODE=transaction`, or lower `?pool_max_conns=`. |
| Uploads answer 503 `STORAGE_UNAVAILABLE` | `SUPABASE_SECRET_KEY` is not set |
| AI routes answer 503 `AI_UNAVAILABLE` | No key for the selected `AI_PROVIDER` |
| Sources stay *processing* | Workers cannot read the queue. Check `REDIS_URL` and `/health/deep` (`redis: false`). On restart, sources stuck in `processing` with no queued job are re-queued. If the job could not be queued at all, the source shows *failed*: delete it and add it again. |
| 503 `SERVER_BUSY` | The instance is at `MAX_IN_FLIGHT`. Add instances or raise the limit. |
