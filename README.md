# pylon — KSeF invoice downloader

A local, resumable Go CLI for downloading original KSeF v2 invoice XML and minimal metadata. It does not issue invoices or create credentials. TEST is the default; production requires `--environment prod` explicitly. The TEST OpenAPI contract was checked against v2.9.0 build `2.9.0-te-20261020.1+9a895dfe72629e58773210e624fc22bd934c4d2a`; recheck the selected environment's contract when upgrading.

## Requirements and credentials

- Go 1.27.2 or the module's supported Go toolchain.
- An authorized taxpayer NIP and an existing KSeF v2 token with `InvoiceRead` for it.
- A private, writable local directory. Filesystem permissions are not encryption: use encrypted disks/backups, restrict access, and apply your retention policy.
- The token must be provisioned outside pylon. See the [official KSeF v2 documentation](https://api.ksef.mf.gov.pl/docs/v2/index.html) and [KSeF token guidance](https://github.com/CIRFMF/ksef-api/blob/main/tokeny-ksef.md).

Supply the secret using `PYLON_KSEF_TOKEN` or a private token file (`chmod 600 /path/to/token`). Never put a token in command-line arguments, source control, or logs. `--token-file` rejects group/other-readable files.

## Build and test

```sh
go test ./...
go build -o ./bin/pylon ./cmd/pylon
./bin/pylon --help
```

## First sync and resume

First run requires an explicit RFC3339 start instant. Set `AUTHORIZED_TEST_NIP` to your authorized synthetic TEST taxpayer's valid NIP:

```sh
export PYLON_KSEF_TOKEN='(provisioned token)'
./bin/pylon sync --environment test --nip "$AUTHORIZED_TEST_NIP" \
  --subject received --from 2025-01-01T00:00:00Z --storage-dir ./data
```

Choose `--subject issued` or `--subject both` as needed. `--to RFC3339` optionally bounds the historical interval. Omit `--from` after a checkpoint exists to resume. Supplying an earlier `--from` replays without moving saved progress backwards. Each subject has an independent cursor; all directions share the invoice XML directory.

Environments are isolated in `data/<environment>/<NIP>/`: TEST (`api-test.ksef.mf.gov.pl`), DEMO (`api-demo.ksef.mf.gov.pl`), and PROD (`api.ksef.mf.gov.pl`). No environment fallback occurs. Checkpoints use permanent-storage time and only advance after the queried window's invoices are saved. A lagging or missing completeness watermark stops the run as incomplete rather than claiming coverage.

## Export invoice due dates to Google Calendar

Export downloaded invoices offline; no KSeF token or Google credentials are needed:

```sh
umask 077 # Calendar exports contain invoice identifiers; keep the file private.
./bin/pylon export-ics --environment prod --nip "$AUTHORIZED_PROD_NIP" \
  --subject received --storage-dir ./data > invoice-due-dates.ics
# Import into a dedicated calendar (requires separately configured gcalcli/OAuth):
gcalcli --calendar 'Invoice payments' import invoice-due-dates.ics
```

Only import the file if pylon exits successfully. Output is ICS only, with no summary
mixed into stdout. The usual environment default is TEST; select production explicitly.
`--subject issued` and `both` are also supported. Export uses the namespace lock,
checks XML/sidecar integrity, and does not change invoices or sync checkpoints.
As with sync, opening storage can create missing namespace directories.

An invoice is included only when its FA(2) or FA(3) XML has exactly one
`Fa/Platnosc/TerminPlatnosci`, containing exactly one valid `Termin` calendar date
and no `TerminOpis`. Missing, description-only, relative, multiple (even identical),
and invalid dates are skipped, as are unknown schemas. Dates are never inferred
from issue dates, free text, or payment periods. This is not full XSD validation.
The payment fields follow the published [FA(3) schema](https://crd.gov.pl/wzor/2025/06/25/13775/schemat.xsd).

Each included invoice creates one private, transparent all-day event on its due date,
with the invoice number in the title and KSeF identity in the description. No amounts,
party names, or bank details are exported. UIDs are stable per environment/NIP/invoice.
No reminders are embedded. All dates, including past dates, are exported; **paid
invoices are not filtered**, and corrections are not reconciled with original invoices.
This is a deadline snapshot, not a payment ledger or synchronization: stable UIDs do
not guarantee Google/gcalcli will update or deduplicate repeated imports. Use a
separate calendar and manage previously imported events when replacing the snapshot.
`--from`, `--to`, and `--token-file` are sync-only and rejected by this command.

## Safety and recovery

- One writer per environment/NIP namespace; a second process fails. The OS lock is released when its owning process exits. Do not manually remove a live lock.
- Supported storage is a private local Linux filesystem with working `flock`, atomic rename, and file/directory `fsync` (for example ext4). Network filesystems, Windows, and filesystems without these guarantees are not supported. Each file and its parent directory is flushed before progress is committed; this does not replace backups or protect against failing hardware.
- XML is stored byte-for-byte, with JSON sidecars and independent checkpoint JSON. Keep all three together in backups. Sidecars retain the first observed `subject` and a `subjects` list covering both directions when applicable; legacy sidecars are reconciled on replay. Local private permissions do not provide encryption.
- Storage paths (including ancestors), XML, sidecars, checkpoints, and locks must not be symlinks. Existing artifact files must be regular and inaccessible to group/others (`chmod 600`). Invalid identifiers, conflicting metadata, or malformed XML fail visibly; do not bypass these checks by advancing checkpoints.
- On failure, retain the data and rerun. Valid existing XML/checksum pairs are skipped; interrupted XML/sidecar pairs are validated and reconciled. Corruption and conflicting bytes fail visibly. If a checkpoint is lost, restart with an explicit earlier `--from` and let the tool deduplicate files. Never edit a cursor forward manually.
- Cancellation and failures return nonzero; only fully completed windows are checkpointed. Logs/summaries avoid printing credentials and invoice content.
- Page size is 250 and bounded date windows are at most 100 days. The first page fixes the HWM-bounded upper end for later pages; a missing or regressing HWM leaves the window uncommitted. A lagging HWM reports the entire deferred interval through the requested end.
- Safe transient operations have at most three retries (challenge: two), exponential backoff starting at 250 ms plus jitter, a 45-second HTTP timeout per attempt, and a five-minute operation deadline. `Retry-After` seconds and HTTP dates are honored without shortening the delay; if the deadline expires while waiting, the run stops rather than retrying early. Authentication has a two-minute overall deadline and polls pending status every 750 ms. Ambiguous authentication start, redemption, and refresh POSTs are not retried automatically; rerun for a fresh authentication challenge after failure.
- Access tokens are refreshed 30 seconds before expiry. An unexpected 401 permits at most one refresh/replay; 403 stops with permission guidance. Repeated truncation timestamps stop with an error; use an officially supported bulk-export workflow for datasets that cannot be paged safely.
- Summaries retain the active subject's counts and last durable checkpoint even on failure. `failed` counts unrecoverable operations (including cancellation), not the size of the undiscovered backlog. `completed_through=none` means no completed checkpoint exists.

Exit status is zero only for complete requested work. Validation, authentication, API, storage, incomplete coverage, and cancellation errors return nonzero. Test smoke tests require operator-authorized credentials and representative invoices; none are run implicitly, and production use is never a test procedure.
