# FACILITATOR.md — Floor Helper Cheat Sheet

> This repo is `arahin-mini`: 1 button on frontend ("Generate Practice") = 1 async workflow on backend.

No conflict with `README.md` (setup guide), `WORKSHOP.md` (speaker script), `SOLUTION.md` (answer key, keep private). This file is **only for facilitators walking the floor**.

---

## 1. Mental model in 30 seconds

```
POST /v1/quiz-requests -> 202 {request_id, processing}  (instant)
GET  /v1/quiz-requests/{id} -> processing | ready+quiz_id | failed+message
GET  /v1/quizzes/{quiz_id} -> quiz WITHOUT correct_option
```

Pipeline inside `internal/quiz/service.go:runGeneration`:

```
Validate -> GetLesson (TODO 1) -> GetMastery (TODO 2) -> buildAIRequest (TODO 3)
-> Call AI stub -> ValidateResponse (TODO 4) -> CreateQuiz tx (TODO 5) + Batch (TODO 6)
```

Wiring: `cmd/api/main.go` = `db -> repository -> service -> handler + aiClient`. If a student is lost, point them to that file first.

Seed IDs (memorize these, you'll type them all day):

```
user   11111111-1111-1111-1111-111111111111 (Alice)
lesson 22222222-2222-2222-2222-222222222222 (Hash Function, mastery 40)
```

---

## 2. Pre-workshop checklist (run on YOUR machine first)

```sh
go version            # need Go 1.25+
pg_isready            # postgres must be running
cp .env.example .env  # then check DATABASE_URL
make migrate; make seed
go test ./...         # must pass on fresh clone (only non-TODO code is tested)
make run              # expect {"msg":"server listening","addr":":8080"}
curl http://localhost:8080/health  # expect {"status":"ok"}
```

---

## 3. The 3 curls you'll repeat per student

```sh
# 1. Start job (always 202, even if code is broken — broken shows up on poll)
curl -X POST http://localhost:8080/v1/quiz-requests \
  -H "Content-Type: application/json" \
  -d '{"user_id":"11111111-1111-1111-1111-111111111111","lesson_id":"22222222-2222-2222-2222-222222222222"}'

# 2. Poll (THIS is your diagnostic — read `message` field)
curl http://localhost:8080/v1/quiz-requests/<request_id>

# 3. Fetch quiz (only works when status=ready)
curl http://localhost:8080/v1/quizzes/<quiz_id>
```

Tip: wait ~1s between step 1 and 2 (`AI_STUB_DELAY_MS=400`). If you poll instantly you'll see `processing` even on correct code.

---

## 4. Diagnose by `failed` message (most important table)

The `message` in step 2 tells you exactly which TODO is still broken:

| Poll returns | Meaning | Send student to |
|---|---|---|
| `load lesson: TODO 1: ...` | `GetLesson` not implemented | `internal/quiz/repository.go:77` |
| `load mastery: TODO 2: ...` | `GetMastery` not implemented | `internal/quiz/repository.go:97` |
| `build AI request: TODO 3: ...` | `buildAIRequest` not implemented | `internal/quiz/service.go:261` |
| `save quiz: TODO 5 + 6: ...` | `CreateQuiz` not implemented | `internal/quiz/repository.go:141` |
| `validate AI response: ...` | TODO 4 missing OR stub fail-rate triggered | `internal/quiz/service.go:196` + check `.env` `AI_STUB_FAIL_RATE` |
| `AI generation: ...` | Stub forced failure (`AI_STUB_FAIL_RATE>0`) | Set it back to `0.0`, restart |
| `status: processing` forever (>5s) | Server not restarted after edit, or goroutine stuck | Restart `make run`, check terminal logs |
| `quiz not found` / `quiz request not found` | Student copy-pasted wrong UUID, or restarted server (jobs are in-memory, wiped on restart) | Re-POST to get fresh IDs, don't restart mid-demo |

Server logs also print `generation failed` with `request_id` — check the speaker's terminal if poll output is confusing.

---

## 5. Environment bugs (80% of floor calls)

| Symptom | Cause | Fix |
|---|---|---|
| `connect to postgres failed` on `make run` | Wrong `DATABASE_URL`, postgres down | `pg_isready`, verify password in `.env`, `createdb arahin_mini` |
| `psql: command not found` | No psql on student laptop | Use pgAdmin / DBeaver to run `migrations/001_init.sql` + `seed/seed.sql` manually |
| `relation "lessons" does not exist` | Skipped `make migrate` | Run `make migrate; make seed` |
| Empty quiz / `lesson not found` on correct code | Skipped `make seed` or seeded wrong DB | Check `DATABASE_URL` DB name matches, re-run `make seed` (idempotent) |
| `port 8080 already in use` / `bind: address in use` | Old `make run` still running | Kill old process (Ctrl+C), or change `HTTP_ADDR=:8081` in `.env` |
| `go: command not found` / old Go version | Go not installed / <1.25 | Point to go.dev, can't proceed without it |
| `.env` committed / shared | Student edited `.env.example` instead of `.env` | `cp .env.example .env`, never commit `.env` (already gitignored) |
| Windows `psql "$(DATABASE_URL)"` quoting issue | Make + PowerShell quoting | Run psql manually: `psql "postgres://..." -f migrations/001_init.sql` |
| `INVALID_REQUEST user_id is required` | Bad curl JSON (missing field, `uuid.Nil`) | Re-copy curl from section 3, check quotes on Windows (`"` vs `'`) |
| `invalid request_id` (400) | Malformed UUID in URL | Copy full `request_id` from POST response |

Reset sequence (safe to run anytime, idempotent):

```sh
make migrate
make seed
go test ./...
make run
```

---

## 6. All 10 steps, floor-style (what / why / error / fix)

> Truth note: the repo only has **6 TODOs**. The 10 steps below = the full
> pipeline in poll order, so you can match any `failed` message to its owner:
> setup (1) + TODO 1-6 (2-7) + async + fetch (8-10).

### Step 1 — Setup: migrate + seed + `.env` (no TODO, but 80% of calls)

- **What:** `make migrate` (creates 5 tables), `make seed` (Alice + hash lesson), `.env` holds `DATABASE_URL`.
- **Why:** without this, correct code still fails. Always check this first.
- **Error:** `relation "lessons" does not exist` / `lesson not found` on correct code / `connect to postgres failed`.
- **Fix:** re-run `make migrate; make seed` (idempotent), verify `DATABASE_URL` DB name, `pg_isready`. Windows without `make`: `psql "postgres://..." -f migrations/001_init.sql`, then seed file.

### Step 2 — TODO 1: `GetLesson` (`internal/quiz/repository.go:77`)

- **What:** `SELECT id, title, objective, created_at FROM lessons WHERE id = $1`, scan into `Lesson`, map `pgx.ErrNoRows` → `ErrLessonNotFound`.
- **Why this exists:** the AI needs the lesson objective as context; parameterized `$1` keeps user input out of SQL (injection safety). Reference example: already-working `GetQuiz` at `repository.go:152`.
- **Poll symptom:** `load lesson: TODO 1: ...`.
- **Common errors + answers:**
  - Raw `no rows in result set` leaks to client → forgot `errors.Is(err, pgx.ErrNoRows)` check. Ask: "what should happen when the ID doesn't exist?"
  - Garbage/wrong fields → `Scan` order doesn't match SQL column order. Tell them to compare line by line.
  - `"... WHERE id = '" + id + "'"` → string-concatenated SQL. Stop them: always `$1`, never paste input into SQL.

### Step 3 — TODO 2: `GetMastery` (`internal/quiz/repository.go:97`)

- **What:** `SELECT user_id, lesson_id, score FROM masteries WHERE user_id = $1 AND lesson_id = $2`, map no-rows → `ErrMasteryNotFound`.
- **Why:** mastery score (Alice = 40) lets the AI tune difficulty. Same `$1/$2` lesson as TODO 1, now with a composite key.
- **Poll symptom:** `load mastery: TODO 2: ...` (means TODO 1 just got fixed — celebrate, move on).
- **Common errors + answers:**
  - Only `WHERE user_id = $1` (missing lesson) → wrong score when a user has many lessons.
  - Same `ErrNoRows` / `Scan`-order bugs as TODO 1.
  - Swapped `userID, lessonID` argument order vs `$1, $2`.

### Step 4 — TODO 3: `buildAIRequest` (`internal/quiz/service.go:261`)

- **What:** `return ai.GenerateQuizRequest{Objective: lesson.Objective, Mastery: mastery.Score}, nil`.
- **Why:** translator between two languages — service speaks `Lesson/Mastery`, AI only understands `Objective + Mastery`. This is the seam that lets the stub be swapped for a real LLM later.
- **Poll symptom:** `build AI request: TODO 3: ...`.
- **Common errors + answers:**
  - `lesson.Title` instead of `lesson.Objective` → quiz about the wrong thing. Ask: "which field describes *what to test*?"
  - Over-engineering difficulty logic → not needed; stub already derives difficulty from mastery. Base 3-line version passes the whole workshop.

### Step 5 — AI stub call (no TODO, but confusing)

- **What:** `aiClient.GenerateQuiz(ctx, aiRequest)` in `service.go` — offline stub, waits `AI_STUB_DELAY_MS`, returns fixed 3-question quiz.
- **Why:** runs offline with no API key; `AIClient` interface means a real provider drops in without touching the service.
- **Error:** `AI generation: ...` (random) → `AI_STUB_FAIL_RATE > 0` in `.env`. Set back to `0.0` and restart. Instant `processing` on first poll is normal (400 ms delay) — wait 1 s and poll again.

### Step 6 — TODO 4: validate AI response (`internal/quiz/service.go:196`)

- **What:** call existing `ai.ValidateResponse(aiResponse)` **before** saving; rules in `internal/ai/client.go:61` (difficulty in easy/medium/hard, at least 1 question, non-empty text, exactly 4 options, `correct_option` in range).
- **Why:** "AI generates. Backend governs." Never persist unvalidated AI output.
- **Poll symptom:** without it the pipeline *appears* to work until bad AI output corrupts the DB; with stub fail-rate on: `validate AI response: ...`.
- **Common errors + answers:**
  - Validation placed **after** `CreateQuiz` → too late, bad rows already saved. Must be step 5, before step 6.
  - Hand-rolled `len(questions) > 0` check only → misses option count / difficulty. Tell them the function already exists and is tested — just call it.

### Step 7 — TODO 5: transaction in `CreateQuiz` (`internal/quiz/repository.go:141`)

- **What:** `tx, err := r.pool.Begin(ctx)` + `defer tx.Rollback(ctx)`, insert quiz with `tx.QueryRow` + `RETURNING`, `tx.Commit(ctx)`.
- **Why:** all-or-nothing — 1 quiz + 3 questions must succeed together; if question #2 fails, rollback leaves no orphan quiz row.
- **Poll symptom:** `save quiz: TODO 5 + 6: ...`.
- **Common errors + answers:**
  - Used `r.pool.QueryRow` instead of `tx.QueryRow` → insert escapes the transaction (breaks atomicity silently).
  - Missing `defer tx.Rollback(ctx)` → failed runs leave half-written rows. It's a safe no-op after successful `Commit`.
  - Early `return` on error without the deferred rollback in place.
  - Empty `params.Questions` not rejected → must error early, never save an empty quiz.

### Step 8 — TODO 6: `pgx.Batch` for questions (same function)

- **What:** `batch := &pgx.Batch{}`, `batch.Queue(INSERT INTO questions ...)` per question with `json.Marshal(question.Options)` for the JSONB column, `results := tx.SendBatch(ctx, batch)`, mandatory `results.Close()` before commit.
- **Why:** 1 quiz + 3 questions = 4 inserts in **1 network round-trip** instead of 4. `Close()` flushes the batch and frees the connection.
- **Poll symptom:** same `save quiz: ...`; silent version: quiz created but with 0 questions, or hangs/leaks connections.
- **Common errors + answers:**
  - Forgot `results.Close()` → #1 silent bug, batch never executes / connection leak.
  - Looped `tx.Exec` per question → works but is 3 round-trips; ask "how many round-trips is that?"
  - Passed `[]string` directly instead of `json.Marshal` → JSONB insert fails (mirror: `GetQuiz` decodes with `json.Unmarshal`, `repository.go:193`).
  - `question.ID` empty / reused → each question needs `uuid.New()`; `Position: i` must be set for `ORDER BY position`.

### Step 9 — Async contract: 202 + poll (`internal/quiz/service.go:115`, `handler.go:46`)

- **What:** POST returns `202` instantly, heavy work runs in `go s.generateQuiz(req)` with `context.Background()`; poll transitions `processing → ready+quiz_id | failed+message`. Jobs live in an in-memory map (wiped on restart; production uses Cloud Tasks + durable table).
- **Why students get confused:** they expect POST to return the quiz. Teaching line: "HTTP request is not the AI operation."
- **Errors + answers:**
  - `processing` forever (>5 s) → server wasn't restarted after the fix, or they poll the wrong `request_id`. Restart, re-POST, re-poll.
  - `quiz request not found` (404) → wrong UUID, or server restarted mid-demo (memory wiped). Re-POST for fresh IDs.
  - `INVALID_REQUEST user_id is required` → bad curl JSON (Windows quoting: use double quotes outside). Re-copy curl from section 3.

### Step 10 — Fetch quiz + hidden answer key (`internal/quiz/repository.go:152`, `model.go`)

- **What:** `GET /v1/quizzes/{id}` returns `id, difficulty, questions[{id, question, options}]` — `correct_option` stays server-side via `json:"-"`.
- **Why:** answer key must never reach the client (cheating + contract safety).
- **Done check:** `ready` + 3 questions x 4 options, **no** `correct_option` field in JSON.
- **Common errors + answers:**
  - Student "fixes" the missing field by removing `json:"-"` → stop them, the omission is the feature.
  - `quiz not found` (404) → polling `quiz_id` before `ready`, or copy-paste error. Poll first, then fetch.
  - `jsonb_array_length(options) = 4` DB error on insert → upstream validation (Step 6) was skipped.

---

## 7. Floor protocol

1. **Reproduce first:** run their 3 curls yourself, read the `message`. Don't read their code blind.
2. **Point, don't type:** tell them file + line (e.g. "look at `repository.go:77`, compare with `GetQuiz` below it"). Let them type the fix.
3. **One TODO at a time:** order is TODO 1 → 2 → 3 → 4 → 5+6. Poll after each fix; the error message should move one step forward.
4. **If stuck >5 min:** show them the shape from `SOLUTION.md` on YOUR laptop only — never on projector, never paste full file. Give hints from `WORKSHOP.md` section 4.
5. **Fast finishers:** set `AI_STUB_FAIL_RATE=0.2` to demo `failed`, or `AI_STUB_DELAY_MS=1500` to watch `processing`, or ask "why is `correct_option` tagged `json:\"-\"`?" (answer: answer key must never leave server).
6. **Done check:** `ready` + `GET /v1/quizzes` returns 3 questions, 4 options each, NO `correct_option` field.

Useful read-only checks (don't edit student code without asking):

```sh
go build ./...   # finds compile errors fast
go vet ./...
gofmt -l .       # non-empty = formatting issue
go test ./...    # starter tests should always pass
```
