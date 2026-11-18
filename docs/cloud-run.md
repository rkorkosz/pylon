# Weekly invoice payment tasks on Cloud Run

The private `SyncInvoices` function discovers **received** invoices using KSeF permanent-storage time. The first run saves the start of the current Warsaw month; subsequent runs resume the same cursor across month boundaries. One explicit FA(2)/FA(3) payment date is required. The task is created **during the run**, due one calendar day before payment. If that date already passed but payment is due today or later, the task is due today; invoices already past the deadline are skipped. Dates are compared in `Europe/Warsaw`. Tasks stores a date, not a time or a guaranteed notification. A weekly scan can miss short deadlines; use a more frequent authorized schedule if that is unacceptable. This workflow does not use Calendar or verify bank payments.

## Owner gate: verify in TEST first

- Google's [runtime table](https://docs.cloud.google.com/run/docs/runtimes/function-runtimes#go) lists `go127`. The function builds locally with Go 1.27; the managed build in your project is **not verified**. Use the private TEST source build below to validate `go.mod`, the entry point and the deployed runtime before enabling invocation. If it fails due to runtime compatibility, do not lower Go silently; agree on the build target first.
- Obtain a KSeF v2 token with only `InvoiceRead`, scoped to the authorized synthetic TEST taxpayer. Check [official token guidance](https://github.com/CIRFMF/ksef-api/blob/main/tokeny-ksef.md) and confirm token authentication is still supported in your target environment and on the rollout date. The app supports only token auth; certificate-only rollout needs a separate implementation. Never reuse TEST credentials or a TEST list in production; there is no environment fallback.
- As the designated Workspace user, create a new list, e.g. **KSeF Invoice Payments (TEST)**, in Google Tasks. Use [tasklists.list](https://developers.google.com/workspace/tasks/reference/rest/v1/tasklists/list) with the same user's authorization and paginate until the new list is found. Save the returned `id`, not its title or `@default`. The runtime service account is not the task owner.
- Enable the Tasks API in the OAuth client's project. Configure a Workspace Internal OAuth app if eligible, confirm admin approval, and use a client and registered redirect URI suitable for the operator's one-time OAuth tool. One option is the [OAuth Playground](https://developers.google.com/oauthplayground/) with **your own** client credentials and the Playground redirect URI registered on that client. Sign in as the intended user, request only `https://www.googleapis.com/auth/tasks`, request offline access (`access_type=offline`) and consent (`prompt=consent` when needed), then exchange the code for a refresh token. Do not depend on the Playground's default client. Verify that the token works after the initial access token expires. For an External app left in Testing, refresh tokens for this scope typically expire after seven days; check [expiration and revocation conditions](https://developers.google.com/identity/protocols/oauth2#expiration) before unattended use. Workspace policy can block or revoke consent.
- Provision the plain KSeF token and a separate UTF-8 Google JSON secret with `client_id`, `client_secret` and `refresh_token`. Enter them through Secret Manager or upload from private files (`chmod 600`, e.g. `gcloud secrets versions add SECRET --data-file=PRIVATE_FILE`); don't paste them into shell arguments, logs, trigger substitutions, or source control. Pin numeric versions in TEST and rotate by adding versions and deploying a new revision. For `invalid_grant` obtain fresh offline consent as the **same** user and check the list ID before redeploying. Do not revoke working old credentials until the new path is tested.

## Projects, IAM and GitHub

Use separate TEST and production projects/databases/secrets/lists where possible. Choose the Cloud Run region and Firestore Native database location before creating Firestore (location cannot be changed in place). Keep them close and conform to retention and data-location requirements. Enable Cloud Run, Cloud Build, Artifact Registry, Cloud Storage, Firestore, Secret Manager, Cloud Scheduler, IAM Credentials, Cloud Logging and Monitoring APIs as needed. Expect billing for builds, stored images/build artifacts, Firestore reads/writes/storage, Secret Manager, invocation CPU/memory, Scheduler jobs and logs. Set a budget and retention policies; do not expire deduplication records while replay is possible.

Create distinct identities in the target project:

| Identity | Access |
| --- | --- |
| Cloud Build trigger/deployer | Cloud Run source deployment ([required roles](https://docs.cloud.google.com/run/docs/deploy-functions#required_roles)), `iam.serviceAccounts.actAs` on the runtime and nested source-build accounts, and Cloud Logging writer. For explicit private IAM in `cloudbuild.yaml`, also grant `run.services.getIamPolicy`, `run.services.setIamPolicy` and permission to enable the invoker check (use a suitable custom role, or Cloud Run Admin in a dedicated TEST project). Ensure it can stage source and submit the nested build. No runtime secret payload access. |
| Nested source-build account | `roles/run.builder` on the project; no runtime data access. |
| Cloud Run runtime | Firestore document access (e.g. `roles/datastore.user` on the dedicated project, constrained to the intended database where supported); `roles/secretmanager.secretAccessor` on the two secrets only. No Tasks IAM role or keys. |
| Scheduler caller | `roles/run.invoker` **only on this service**. Retain the Google-managed Scheduler service agent role so Scheduler can mint its OIDC token. |

In Cloud Build, connect GitHub with the GitHub App and grant the connection access only to [`rkorkosz/pylon`](https://github.com/rkorkosz/pylon). Create a trigger for pushes to `^main$` using `cloudbuild.yaml` and the dedicated trigger service account. Protect `main` and require the existing GitHub Actions CI check. Do not create service-account keys or store GCP keys in GitHub. Set these non-secret trigger substitutions:

| Substitution | Example / purpose |
| --- | --- |
| `_ENVIRONMENT`, `_REGION`, `_SERVICE` | `test`, the selected region, `pylon-test` (stable deployment namespace) |
| `_RUNTIME_SA`, `_SOURCE_BUILD_SA` | Runtime service account email and source-build account resource `projects/PROJECT/serviceAccounts/ACCOUNT_EMAIL` |
| `_FIRESTORE_DATABASE` | `(default)` or explicitly provisioned Native database ID in the target project |
| `_NIP`, `_TASK_LIST` | Authorized TEST NIP and dedicated list ID |
| `_KSEF_SECRET`, `_GOOGLE_SECRET` | Secret **names** with numeric versions, e.g. `pylon-test-ksef:1` (not payloads) |

Deploy an approved TEST merge. The config enables the invoker IAM check, denies public access, and uses scale-to-zero, concurrency 1 and at most one instance per revision. Do not grant `roles/run.invoker` to `allUsers` or `allAuthenticatedUsers` (including inherited permissions). POST anonymously and verify 401/403; only authenticated identities explicitly granted invocation should reach the function. The handler runs for at most 12 minutes (with up to 3 seconds for lease cleanup), Cloud Run for 14 minutes, and Scheduler for 16 minutes. The Firestore lease lasts 15 minutes even if the process crashes and overlaps another revision. A run checkpoints at up to 24-hour segments, and discovery has a six-minute budget so pending work still gets time. Measure TEST throughput; rerun authenticated manual invocations until bootstrap catches up. If one complete segment still cannot fit or a watermark continuously lags, investigate rather than advancing the cursor manually or enabling unattended scheduling.

## Scheduler and TEST sign-off

Set `SERVICE_URL` to the canonical Cloud Run URL (no path), `SCHEDULER_SA` to the caller email and `SCHEDULER_REGION` to the chosen Scheduler location. As an authorized operator, after granting that identity service-level invocation:

```sh
gcloud scheduler jobs create http pylon-test-weekly --location="$SCHEDULER_REGION" \
  --schedule='0 8 * * MON' --time-zone=Europe/Warsaw --http-method=POST \
  --uri="$SERVICE_URL/" --oidc-service-account-email="$SCHEDULER_SA" \
  --oidc-token-audience="$SERVICE_URL" --attempt-deadline=960s \
  --max-retry-attempts=4 --min-backoff=240s --max-backoff=900s --max-retry-duration=3600s
```

Run the TEST job manually with `gcloud scheduler jobs run pylon-test-weekly --location="$SCHEDULER_REGION"`. Use only synthetic, authorized TEST invoices. Check the durable cursor, inserted task count and date in the designated user's Google Tasks client; verify the date survives an API read and is displayed correctly in the intended timezone. Confirm pagination and `showCompleted=true`/`showHidden=true`. Complete a task, rerun, and verify it remains completed without a duplicate. Delete a synthetic task and verify it is not reported as paid and is not recreated. Redeploy and rerun to confirm lease, checkpoint and deduplication survive revisions. Verify indefinite OAuth refresh suitability and test alerts and rollback in TEST. The 204 response has no body; incomplete work returns 503, and rejected credentials/configuration return 500. Scheduler retries non-2xx responses within its configured bounds; treat permanent 500s as operator incidents, not recovery by repeated retries.

Set up alerts on the service's structured logs: alert immediately on `jsonPayload.counts.unresolved_intents > 0`, `jsonPayload.counts.authorization_error=true`, or `jsonPayload.message="invoice_sync_failed"` with `jsonPayload.reason="configuration"` or `"google_secret"`. Alert on repeated non-2xx Scheduler attempts / incomplete runs. Use a log-based counter for `jsonPayload.message="invoice_sync" AND jsonPayload.complete=true` scoped to the service and a metric-absence alert longer than one week (e.g. eight days); validate notification delivery in TEST. Never attach invoice data or secrets to alerts.

## Recovery and production approval

Inspect `pylonRuns/{namespace}` in Firestore: `bootstrap` never advances; `cursor` is the last covered instant (stored as RFC3339 with full precision); `owner`/`expires` fence writes. A namespace is scoped to the stable deployment name, KSeF environment, taxpayer and destination list; changing these or the Firestore project/database can produce duplicate tasks. Keep them stable across revisions and rotations. `intents` records are keyed by an opaque hash; only minimal invoice reference suffixes (without issuer NIP), dates, integrity metadata and delivery state are stored, not XML. Restrict access. On ordinary failures, replay by invoking again without editing the cursor. Do not clear an active lease or delete recorded IDs.

A successful insert records the returned ID. A timeout, server failure or process crash may have created a task even without a recorded ID. A later run lists all pages and reconciles an exact `pylon:v1:…` notes marker with a single live task, including completed/hidden tasks; `completed` indicates paid, not absence, deletion or an open task. A known ID is terminal even if the task was deleted. The system deliberately prefers an alert and a potentially missing task to an automatic duplicate; it is not exactly-once across these two APIs. If there is no unique match, pause Scheduler, wait for all active requests and the lease to end, check the configured user/list (including hidden/completed entries), the marker, and any available history with an authorized operator. If a unique live match is found, rerun for automatic reconciliation. An empty scan alone does **not** prove no insertion occurred. Only if an operator establishes it is safe to retry should they explicitly clear `claim` and set an unresolved intent with no `taskId` to `pending` together in Firestore. Otherwise leave it unresolved; document and review the incident before resuming. Do not set a paid flag or recreate deleted tasks automatically.

For production, get separate approval after TEST sign-off. Deploy privately with a new production list/database/identity and keep Scheduler absent (or paused if already provisioned) until approved. An authorized caller may POST to `/preview` using an ID token for the **service URL**: this reads KSeF as for a fresh bootstrap, writes neither Firestore nor Tasks, and logs only coverage and counts (`invoice_preview`). Review `complete`, `coverage_start`, `covered_through` and `would_create` against expectations before the first approved production write. Repeat preview if the month or inputs change; it is a snapshot and volumes may change before the write. To roll back, pause Scheduler, finish or let existing leases expire, then route traffic to the last compatible revision (e.g. `gcloud run services update-traffic SERVICE --region=REGION --to-revisions=REVISION=100`). Preserve Firestore and the list; rollback does not undo created tasks.

## Local verification (no cloud credentials or writes)

`go test ./...` runs handler and fake-API tests. To check the registered Functions Framework entry point without credentials, run `PORT=8691 go run ./cmd/pylon-function` and, from another shell, `curl -i http://127.0.0.1:8691/preview` (expect 405, `Allow: POST`, and an empty body). Do not send a POST from this smoke test.

For transaction tests run the Firestore emulator on loopback (or use `gcloud beta emulators firestore start --host-port=127.0.0.1:8689 --project=demo-pylon`):

```sh
docker run --rm --name pylon-firestore-emulator -p 127.0.0.1:8689:8080 \
  gcr.io/google.com/cloudsdktool/google-cloud-cli:emulators \
  gcloud beta emulators firestore start --host-port=0.0.0.0:8080 --project=demo-pylon
# In another shell, after the emulator reports that it is listening:
FIRESTORE_EMULATOR_HOST=127.0.0.1:8689 go test ./... -count=1
```

The emulator tests use disposable namespaces and do not contact KSeF or Google Tasks. The emulator is not a substitute for the authorized TEST build and integration checks above.
