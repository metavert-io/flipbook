# Production Readiness Review — Flipbook (Penninn Midlun)

Date: 2026-11-09 · Scope: full repository (`main` @ f43aec5, clean tree, in sync with `sturla/main`)
Environment: Go 1.25.5 (vendored toolchain), Windows dev host, Fly.io production (`flipbook-metavert`, iad)

## Verified healthy

- `go build ./...` — passes
- `go vet ./...` — clean
- `go test ./...` — **15/15 packages pass** (auth, config, converter, database, distribution, engagement, handlers, integrations, models, publishing, sales, storage, worker, analytics)
- `node --test scripts/admin-detail-polling.test.js` — 6/6 pass
- Git tree clean, nothing unpushed, `.env`/`.env.local`/`.env.docker`, `data/`, `bin/`, `lastsaas/` correctly gitignored

**Architecture strengths worth keeping:** worker leasing + heartbeats + permanent-failure classification; HMAC-signed access grants with constant-time comparison and share-token proof binding; bcrypt with dummy-hash timing parity; session invalidation on password change; Stripe client with HTTPS enforcement, redirect blocking, response caps and idempotency; webhook signature verification with timestamp tolerance; atomic upload writes with extension allowlist and digest; `/readyz` Mongo ping check wired into Fly health checks; login throttling and lead-form rate limiting; bounded analytics event ingestion; html/template autoescaping throughout, no `innerHTML` in `viewer.js`/`admin.js`.

---

## Findings

### Must fix before / at launch

**F1 — ✅ FIXED 2026-11-09: stale `FRONTEND_URL` in `deploy/supervisord.conf`.**
The `lastsaas` program's `environment=` overrode the Fly container env, so all LastSaaS-generated links pointed at the abandoned `flipbook.metavert.io` domain. Now set to `https://bok.bodberi.com` in both `deploy/supervisord.conf` and `fly.saas.toml` (both occurrences of the same variable must stay in sync).

**F2 — ✅ FIXED 2026-11-09: secrets in the Docker build context.**
`.dockerignore` now excludes `.env` and `.env.*`, so env files with secrets no longer reach the build context or the `COPY . .` builder stage in `Dockerfile.saas`.

**F3 — ✅ FIXED 2026-11-09: API key no longer logged at startup.**
`main.go` now logs only the key's provenance via `apiKeyStatus()` (`configured` / `auto-generated` / `not configured`) in both admin and SaaS mode, instead of the secret value.

**F4 — `internal/saas` has zero tests.**
JWT validation (`jwt.go`) and tenant membership middleware (`middleware.go`) are the entire authorization backbone of SaaS mode, and nothing exercises them. The jwt library guards `alg` confusion correctly and expiry is enforced, but an unsigned-expiry token (`ExpiresAt == nil`) is accepted, and membership grants full access with no role distinction (any tenant member can delete flipbooks — see F6). Add table-driven tests: expired token, wrong secret, `alg=none`, missing `X-Tenant-ID`, non-member, deactivated tenant.

### Should fix soon after launch

**F5 — ✅ FIXED 2026-11-09: graceful shutdown now has a 30s deadline.**
`srv.Shutdown` runs under `context.WithTimeout(30s)` and force-exits with a log line if draining times out, instead of blocking forever on a hung connection.

**F6 — `saas_api.go` is a generation behind the v1 API.**
- `UpdateFlipbook` decodes JSON with no body limit (v1 uses `decodeAPIJSON` with a 2 MB cap and `DisallowUnknownFields`).
- No title/slug length validation on upload (v1 caps at 500/200).
- `DeleteFlipbook` ignores errors from `h.db.DeleteFlipbook(id)` / `h.storage.DeleteFlipbook(id)` and returns 204 regardless.
- GridFS backup runs in a goroutine with `context.Background()` and no timeout — a wedged upload leaks a goroutine per request.
- No role check within a tenant: any member can delete any flipbook.
Consider routing SaaS uploads through `createFlipbookFromSource` or backporting its validations.

**F7 — `/api/flipbooks/{id}/status` is unauthenticated (`main.go:372`).**
Deliberate ("upload progress tracking"), and `FlipbookStatus`'s legacy fallback identity returns owner-level access. Anyone with a flipbook UUID can read its status, slug, and error messages — and the slug reveals the viewer URL for unlisted publications. UUIDs are unguessable in practice, so the risk is low, but consider scoping the public response (status only) unless the caller is authenticated.

**F8 — Non-strict config loading in production (`main.go:152`).**
`config.Load()` silently ignores a missing/unparseable config file and falls back to defaults. A typo in `config.prod.yaml` could boot the server with wrong settings (or generated secrets). Use `LoadWithError()` at startup, or at minimum log loudly when `FLIPBOOK_CONFIG` is set but unreadable. Related: without `FLIPBOOK_SESSION_SECRET` set persistently, every restart invalidates all sessions and access grants; enable `security.require_persistent_secrets: true` in prod config.

**F9 — Memory ceiling is tight for conversion workloads.**
`fly.saas.toml` runs Caddy + flipbook + LastSaaS + LibreOffice on one `shared-cpu-2x` / 2 GB machine; the admin-mode `fly.toml` uses 1 GB with LibreOffice. Large PPTX→PDF conversions plus pdftoppm at 300 DPI can push past that under concurrent uploads. Watch OOMs in the first weeks; consider 4 GB or a separate worker machine. `fly.toml` also has no health checks at all — port the `/readyz` check block from `fly.saas.toml`.

### Minor / hardening

**F10 — Non-constant-time API key comparison** in the legacy `apiAuth` middleware (`main.go:693`). Use `crypto/subtle.ConstantTimeCompare`. (The scoped API-key path in `api_auth.go` is fine — keys are looked up by hash.)

**F11 — `set-password` reads via `fmt.Scanln`** (`main.go:83`): password echoes to the terminal and breaks on spaces. Use `term.ReadPassword` (`golang.org/x/term`).

**F12 — X-Frame-Options/CSP interaction on embeds.** Global middleware sets `X-Frame-Options: SAMEORIGIN` on every response; publishing decisions add `frame-ancestors` CSP. Modern browsers prefer CSP and ignore XFO, so embedding works today, but old Safari versions will block embeds, and the README's "ALLOWALL" claim no longer matches the code. Explicitly `Header().Del("X-Frame-Options")` when the decision allows framing.

**F13 — Non-deterministic frontend build.** `Dockerfile.saas` and `scripts/build-frontend.sh` run `npm install --legacy-peer-deps || npm install`; the overlay never runs `npm ci` against `lastsaas/frontend/package-lock.json`. Use `npm ci --legacy-peer-deps` for reproducible images.

**F14 — SPA `index.html` re-read per request** (`main.go:662`). Cache the file (or bytes) at startup; re-read only on failure. Minor perf on every page load.

**F15 — Repo hygiene leftovers:** `bin/flipbook.exe~` backup binary; `data/tmp/` inside the repo holds the Go toolchain/build cache (gitignored, works, but fragile — document it or move it); module path `github.com/metavert-io/flipbook` and Fly app name `flipbook-metavert` still carry the old identity (already tracked in MEMORY.md — fine to defer).

**F16 — ✅ FIXED 2026-11-11: large admin uploads were cut off by the edge proxy.**
The live host's edge proxy (Traefik v3 behind Coolify) aborts any request whose body takes longer than 60 seconds to arrive (`entryPoints.*.transport.respondingTimeouts.readTimeout` defaults to `60s`); verified against production with a throttled upload that returned `502` at 60.8 s. The admin UI sent each document as one multipart request, so a 177 MB catalogue needed ~24 Mbit/s of upstream bandwidth to finish in time, and the client then waited up to 30 minutes with no feedback. The admin upload queue and the "Replace source document" form now send documents as resumable chunks through `/admin/uploads` (session + CSRF header): chunks are sized from measured throughput to take ~8 s, a chunk with no progress for 30 s is aborted and retried, and retries resume from the offset the server confirms. Sessions live under `<data>/flipbooks/.upload-sessions/`, expire after 24 h, and are capped at 10 per user. The no-JavaScript form post and the `/api/flipbooks` API upload still use a single request and remain subject to the proxy limit — raise the proxy read timeout too (e.g. `--entrypoints.https.transport.respondingTimeouts.readTimeout=30m` in the Coolify proxy configuration).

---

## Launch verdict

The codebase is in genuinely good shape: it builds, vets, and tests clean, and the core security machinery (sessions, grants, payments, uploads, conversion sandboxing) is well engineered. **F1, F2, F3 and F5 are fixed as of 2026-11-09** (verified: `go build`, `go vet`, full test suite green). F4 was accepted for now given internal-only use. Schedule F6 for the first hardening sprint, and keep an eye on memory (F9) and the public status endpoint (F7) once traffic arrives. Note: F1's domain change takes effect on the next deploy; `FLIPBOOK_BASE_URL` in `fly.saas.toml` still points at `flipbook.dev.nextdigital.is` and should move to `bok.bodberi.com` at cutover (it drives canonical URLs, cookies, and Stripe/OAuth redirects).
