# Go KSeF invoice downloader — MVP execution plan

## Goal and scope

Build a Go application that authenticates to KSeF API v2, discovers invoices for one taxpayer, downloads their original XML, and stores them locally. A successful repeat run must not create duplicates or silently skip invoices.

**In scope**
- A command-line application, run manually or by an external scheduler.
- One configured taxpayer NIP and one KSeF environment per invocation.
- Received invoices (`Subject2`) and issued invoices (`Subject1`), selected explicitly; proposed default: received invoices.
- Initial historical download and resumable incremental synchronization.
- Original XML files, minimal metadata, durable synchronization checkpoints, and useful logs/exit codes.
- Test environment first; production requires explicit configuration and operator authorization.

**Non-goals**
- Invoice issuance, corrections, sending invoices, UPO retrieval, payment/accounting workflows.
- Web UI, HTTP service, built-in scheduler, multi-tenant service, cloud storage, or database server.
- PDF generation, business-level XML parsing, XSD validation, and invoice analytics.
- Bulk encrypted exports, certificate/XAdES authentication, automatic token creation, and permission management in this MVP.

## Current state and evidence

- `go.mod`: module `github.com/rkrokosz/pylon`, Go `1.27.2`.
- `README.md`: only the project title.
- `AGENTS.md`: points to the official KSeF API v2 documentation.
- No application code, tests, storage format, or existing verification pipeline.
- Planning baseline: installed `go version` is `go1.27.2 linux/amd64`; no implementation tests exist to run.
- Official production OpenAPI was inspected alongside Ministry of Finance documentation. Important observed constraints:
  - Metadata search and XML retrieval require `InvoiceRead`.
  - Metadata pages contain 10–250 records; each filter set has a 10,000-record technical limit.
  - Date filters currently allow at most 100 days per request.
  - Incremental queries should use `PermanentStorage` and ascending order, not invoice issue date.
  - Metadata queries support `restrictToPermanentStorageHwmDate` and return `permanentStorageHwmDate`, a completeness watermark.
  - Bulk exports are officially recommended for larger incremental integrations; individual XML downloads are a deliberate low-volume MVP tradeoff.
- No authenticated calls, credentials, invoice downloads, or production actions were performed during planning.

## Proposed decisions and prerequisites

These are proposed product decisions, not existing requirements. Confirm them in Phase 1 before implementing the affected behavior:

| Decision | Proposal and rationale |
|---|---|
| Interface | CLI named `pylon`; no service deployment needed. |
| Authentication | Operator supplies an existing KSeF v2 token with `InvoiceRead` for the selected NIP. Simplest initial integration; validate token availability and supported lifetime before committing to this approach. |
| Storage | Local filesystem with XML plus JSON sidecars/checkpoints. No database dependency for a single-process downloader. |
| Invoice selection | Default received invoices; optionally issued or both, with independent cursors. |
| History | Require an explicit UTC start time on first sync; no hidden historical cutoff. |
| Concurrency | One sync writer per environment/NIP storage namespace; sequential requests initially. |
| Deployment | Local Linux filesystem initially; document supported locking and durability semantics. |

**Blockers:** If token authentication is unavailable for the intended rollout, stop and revise the authentication phase for a supported certificate flow. If a UI, remote storage, high-volume ingestion, or concurrent writers are required, revise the scope before implementation rather than silently expanding it.

Operator prerequisites: authorized NIP, appropriate KSeF environment, valid externally provisioned credential, `InvoiceRead`, writable private storage, and representative test invoices. Do not commit secrets or real invoices.

### API contract and security boundaries

- Recheck the selected environment's OpenAPI when implementation begins; TEST can contain release-candidate changes not present in production. Record the version/revision used for fixtures.
- Environment base URLs: `https://api-test.ksef.mf.gov.pl/v2`, `https://api-demo.ksef.mf.gov.pl/v2`, `https://api.ksef.mf.gov.pl/v2`.
- Default to TEST; never silently switch environments or send credentials to arbitrary configured hosts. Use injected transports/base URLs only in tests.
- Supply the KSeF credential via `PYLON_KSEF_TOKEN` or a private token file, never a command-line token argument. Access/refresh tokens remain in memory.
- Keep TLS verification enabled. Validate same-origin redirects or reject redirects so credentials cannot leak.
- Logs must exclude credentials, authorization headers, raw invoice XML, and full response bodies containing taxpayer data.
- Private directories/files (proposed `0700`/`0600`); no automatic deletion. Filesystem permissions are not encryption: document disk encryption, backup, retention, and access control as operator responsibilities.

## Proposed implementation layout

All paths below are **new**, except existing `go.mod` and `README.md`:

```text
cmd/pylon/main.go               CLI wiring, signals, exit codes
internal/config/               environment/configuration validation
internal/ksef/                 typed API client, authentication, HTTP policy
internal/storage/              XML/metadata persistence, checkpoints, locking
internal/sync/                 date windows, pagination, durable progress
internal/.../*_test.go          unit and HTTP-fixture tests
internal/ksef/testdata/         synthetic API responses and XML only
.env.example                   variable names/placeholders, no credentials
.gitignore                     local secrets, downloaded data, build output
```

Prefer the Go standard library. Introduce dependencies only for a demonstrated need. Keep API types limited to the endpoints used; do not generate an entire SDK.

## Phase 1 — Confirm MVP contract and create the CLI skeleton

### Changes
- [ ] Confirm the proposed decisions and operator prerequisites above; resolve blockers before dependent work.
- [ ] Add `cmd/pylon`, config validation, and a documented CLI contract:
  - `pylon sync --environment test --nip <NIP> --subject received --from <RFC3339> --storage-dir ./data`.
  - `--subject issued|both` supported; `--to <RFC3339>` optional for bounded historical runs.
  - Subsequent runs without `--from` resume stored progress; an explicit earlier `--from` performs a replay without regressing the saved cursor.
- [ ] Reject malformed NIP/date values, inverted ranges, missing first-run start time, missing credentials, and incompatible checkpoint namespaces before network calls.
- [ ] Define exit codes: zero for complete requested work; nonzero for validation, authentication, incomplete sync, storage failure, or cancellation.
- [ ] Add `.gitignore`, `.env.example`, and README setup/build instructions. Preserve the module path and Go version unless a verified compatibility reason requires change.

### Acceptance criteria and verification
- [ ] P1-AC1: Binary builds; help documents all flags and credential sources without requiring network access.
- [ ] P1-AC2: Table-driven configuration tests cover invalid inputs and environment selection; TEST is default and production is opt-in.
- Proposed commands from repository root: `go build -o ./bin/pylon ./cmd/pylon`, `./bin/pylon --help`, `go test ./internal/config/...`.

## Phase 2 — Authenticate and make bounded authenticated requests

### Prerequisite
Phase 1 decisions approved; token-based authentication confirmed for the target environment and rollout.

### Changes
- [ ] Implement the documented flow:
  1. `POST /auth/challenge`.
  2. `GET /security/public-key-certificates`; select a currently valid RSA certificate intended for `KsefTokenEncryption`.
  3. Encrypt UTF-8 `{tokenKSeF}|{timestampMs}` using RSA-OAEP SHA-256/MGF1 SHA-256; Base64 encode. Use the challenge's timestamp in Unix milliseconds, not the local clock.
  4. `POST /auth/ksef-token` with challenge, NIP context, and encrypted token.
  5. Poll `GET /auth/{referenceNumber}` using the returned authentication token, with deadline and terminal-failure handling.
  6. `POST /auth/token/redeem` to obtain access/refresh tokens; use each token only for its documented purpose.
- [ ] Refresh with `POST /auth/token/refresh` before expiry; handle a rejected access token with at most one controlled refresh/replay. Permanent authorization failures must stop clearly.
- [ ] Implement context cancellation, request timeouts, bounded polling, and capped retry/backoff with jitter for safe transient operations; honor `Retry-After` on 429.
- [ ] Do not blindly retry one-time token redemption or other ambiguous authentication POSTs. Recover through a fresh challenge when the contract requires it.
- [ ] Decode API errors into safe actionable messages (including missing permissions) without dumping sensitive bodies.

### Acceptance criteria and verification
- [ ] P2-AC1: `httptest.Server` fixtures verify request sequence, token placement, encryption decryptability with a test private key, certificate usage/expiry selection, and timestamp precision.
- [ ] P2-AC2: Tests cover pending/success/failed authentication, refresh, 401/403, 429, transient 5xx, deadlines, cancellation, and redaction; retries terminate.
- Proposed command: `go test ./internal/ksef/...` (no live credentials required).

## Phase 3 — Store original XML safely and idempotently

### Changes
- [ ] Implement storage namespace `data/<environment>/<nip>/`, with XML and sidecar metadata under `invoices/<ksefNumber>.xml` and `<ksefNumber>.json`.
- [ ] Treat KSeF numbers as identifiers, never arbitrary paths; validate against the documented format and reject traversal/symlink escapes. Do not derive paths from user invoice numbers.
- [ ] Preserve response bytes exactly; reject empty, malformed, oversized, or non-XML error bodies before committing. Use bounded streaming and context cancellation; do not reserialize XML or resolve external entities.
- [ ] Record KSeF number, permanent storage date, selected subject provenance, download time, byte length, local SHA-256, and API invoice hash when available. Compare the API hash only according to its documented algorithm/encoding.
- [ ] Write private temporary files on the same filesystem, flush and atomically rename. Commit the metadata marker only after XML is durable. Never overwrite an existing different XML silently.
- [ ] Verify existing XML/checksum before skipping; recover interrupted XML/sidecar pairs by validating/reconciling or safely re-downloading. Conflicts/corruption fail visibly without advancing progress.
- [ ] Add a namespace-level OS-backed lock released on process exit; a second writer fails with a clear message. Atomic checkpoint writes use a versioned JSON format.

### Acceptance criteria and verification
- [ ] P3-AC1: Stored XML is byte-identical; replay creates no duplicate and does not replace valid files.
- [ ] P3-AC2: Tests inject interrupted writes, missing sidecars, corruption, permission errors, path traversal, cancellation, and competing writers; no partial artifact is reported as complete.
- Proposed command: `go test ./internal/storage/...` using temporary directories only.

## Phase 4 — Discover and synchronize all matching invoices

### Prerequisites
Authenticated client and durable storage verified in Phases 2–3.

### Changes
- [ ] Use `POST /invoices/query/metadata` with `Subject2` (received) or `Subject1` (issued), `dateType=PermanentStorage`, `sortOrder=Asc`, and `restrictToPermanentStorageHwmDate=true`; download each discovered document with `GET /invoices/ksef/{ksefNumber}`.
- [ ] Partition historical ranges into windows of at most the documented 100-day UTC limit. Set an explicit upper bound per window and use the returned completeness watermark to determine the completed boundary; do not checkpoint wall-clock time or the last invoice's issue date.
- [ ] Fix each window's effective upper bound after its first response to avoid moving result sets during pagination. Preserve API timestamp precision with `time.Time`/RFC3339Nano.
- [ ] Start `pageOffset=0`; increment by one page when `hasMore=true` and `isTruncated=false`. Stop at `hasMore=false`.
- [ ] When results are truncated, follow the documented inclusive restart from the final record's permanent storage date and reset `pageOffset=0`. Deduplicate by KSeF number. Detect repeated boundaries/no progress, including excessive equal timestamps; fail visibly with the cursor unchanged rather than claiming completeness. Bulk export is the follow-up for unpageable/high-volume cases.
- [ ] Persist independent cursors by environment/NIP/subject/filter version only after every invoice in a completed window is durable. A failure leaves that window uncommitted; replay skips validated files. Empty completed windows may advance to their API-confirmed boundary.
- [ ] Reuse inclusive checkpoint boundaries without adding a time epsilon. Handle absent/regressing watermarks conservatively: stop without advancing. If HWM is behind the requested end, finish at the stable boundary and report deferred coverage explicitly rather than claiming the entire end date was synchronized.
- [ ] `both` runs maintain independent received/issued progress while sharing invoice storage; cancellation/failure never advances the affected unfinished window. Earlier replay never moves a stored cursor backwards.
- [ ] Print a summary: discovered, downloaded, already present, failed, completed-through boundary, and deferred interval. Stop on the first unrecoverable failure with a nonzero exit and safe resume instructions.

### Acceptance criteria and verification
- [ ] P4-AC1: Synthetic multi-page, multi-window, empty, truncated, and equal-timestamp datasets produce exactly the expected unique XML files.
- [ ] P4-AC2: Injecting failure after any document or before checkpoint commit, then rerunning, loses no invoice and advances no cursor prematurely.
- [ ] P4-AC3: Tests cover moving/lagging/missing HWM, inclusive boundaries, more than 10,000 matches, no-progress detection, separate subjects, replay, expired credentials, and cancellation.
- Proposed command: `go test ./internal/sync/...`; deterministic HTTP fixtures and temporary storage only.

## Phase 5 — End-to-end proof and operator handoff

### Changes
- [ ] Add an offline CLI integration test combining authentication, pagination, XML retrieval, storage, interruption, restart, and a second no-op sync.
- [ ] Document credential provisioning prerequisites (link official guidance), TEST/DEMO/PROD separation, both invoice directions, first sync, resumption, history replay, permissions, exit codes, lock behavior, and retry limits.
- [ ] Document recovery: keep data after failure; release only abandoned OS locks by ending the owning process; reconcile incomplete artifacts; never edit cursors forward manually. If a checkpoint is lost, replay from an explicit earlier start and deduplicate existing files. Back up XML, sidecars, and checkpoints together.
- [ ] Add CI for formatting, vet, tests/race detection, and build using the module's supported Go toolchain.
- [ ] With operator authorization, run a TEST-environment smoke test against existing synthetic invoices. Confirm expected KSeF numbers independently from the test UI or prepared fixture manifest. Any creation of invoices/test permissions is separate authorized setup, not downloader behavior.

### Acceptance criteria and verification
- [ ] P5-AC1: Offline end-to-end tests prove byte preservation, idempotency, and crash-safe resume without external services.
- [ ] P5-AC2: Authorized TEST run downloads the expected XML; an immediate repeat creates no duplicate files; no secret appears in logs or tracked files. Record credential-dependent checks as not run if credentials are unavailable.
- [ ] P5-AC3: A new operator can build and execute a TEST sync using README instructions.

## Final verification

Proposed commands from repository root, once implementation exists:

```sh
gofmt -l ./cmd ./internal        # must print no files
go vet ./...
go test ./...
go test -race ./...
go build -o ./bin/pylon ./cmd/pylon
```

Live checks require explicit authorization and environment-specific credentials. Never run a production smoke test implicitly.

MVP requirement mapping: connection/authentication → Phase 2; discovery/XML download → Phase 4; durable local storage → Phase 3; usable application → Phases 1 and 5; repeatable safe synchronization → Phases 3–5.

## Execution notes

- This document is the plan only; no implementation phase is complete.
- Implement only the requested phase/range and verify its acceptance criteria before reporting completion.
- Record blockers, contract changes, and checks not run. Do not introduce bulk export, a database, a UI, or a different authentication flow without revising scope.

## References

- Repository: `AGENTS.md`, `go.mod`, `README.md`.
- Official production API documentation: https://api.ksef.mf.gov.pl/docs/v2/index.html
- Machine-readable production contract: https://api.ksef.mf.gov.pl/docs/v2/openapi.json
- Authentication: https://github.com/CIRFMF/ksef-api/blob/main/uwierzytelnianie.md
- KSeF tokens: https://github.com/CIRFMF/ksef-api/blob/main/tokeny-ksef.md
- Environments: https://github.com/CIRFMF/ksef-api/blob/main/srodowiska.md
- Download/pagination: https://github.com/CIRFMF/ksef-api/blob/main/pobieranie-faktur/pobieranie-faktur.md
- Completeness watermark: https://github.com/CIRFMF/ksef-api/blob/main/pobieranie-faktur/hwm.md
- Recommended bulk incremental approach (future scaling reference): https://github.com/CIRFMF/ksef-api/blob/main/pobieranie-faktur/przyrostowe-pobieranie-faktur.md
