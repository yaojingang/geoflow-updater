# Host recovery contract

Updater protocol 5 owns recovery generations outside the application snapshot.
All full restores use one `RestoreRequest` containing the recovery point and
stable operation ID. Manual restore, update failure recovery, and maintenance
conversion recovery share the same Store barrier.

## Durable boundaries

Host authority lives under `/var/lib/geoflow-updater/recovery-control/primary`.
Only its `public` directory is bound read-only at
`/run/geoflow-recovery-control` in every PHP service, including init and both
application slots. The signed Compose document remains unchanged; a generated
runtime overlay adds the mount and `GEOFLOW_RECOVERY_CONTRACT=1`.

After archive validation and staging, the Store persists `restoring` and a new
epoch before replacing configuration, files, or database contents. File and
parent-directory fsync complete before restoration proceeds. Retrying the
current operation keeps its epoch. A different explicit restore starts a new
epoch. A completed data restore cannot replay within the same transaction.
Lost or corrupt initialized authority stops recovery.

Updates freeze an opaque administrator digest in a private checkpoint before
migration or topology changes. Manual restores inspect the current source before
restoring it. The digest must match the restored administrator set; a mismatch
requires operator investigation and leaves maintenance active.

## Isolation and evidence

Successful data restoration enters `validating`. Only PostgreSQL, Redis, and a
fixed PHP entry point run. The entry point bypasses the image entrypoint and
automatic migration, installation, and optimization. Core preparation must
invalidate restored credentials, verify administrator and theme identities,
and record held database intent evidence. An independent verification response
must match the preparation response.

The host retains both the pre-restore and recovery-point Redis archives with
point, epoch, and transaction provenance. A fixed Redis program quarantines all
configured local Redis queue keys, including delayed, reserved, notification,
and unknown queue states. Payloads remain in Redis without expiry; manifests
retain original expiry, type, and content digests. Unrelated Redis keys remain
unchanged. Unsupported Redis topology or changed evidence stops recovery.

Only durable Core and Redis proofs permit `http_ready`. HTTP then supports
reading, authentication, and permitted operations; business mutations and all
background services remain held. Health probes cannot release that hold.
Protocol 5 provides no automatic transition from restored `http_ready` to
`ready`. Explicit reconciliation of database intents, external receipts,
queued work, and themes remains a separate operation.

## Fixed Core 3.1.0 adapter

The embedded legacy adapter is restricted to the signed app image
`ghcr.io/yaojingang/geoflow-app@sha256:94c01bfd52941cf48ad9693fce30fc571c29353189fb5e128d0e0752daa36448`
and source commit `6c963783bbf49f0b5ec9d0121a924ee54005b60d`.
Its bootstrap checksum and PostgreSQL schema are checked before preparation.
The image identity is covered by a test verifying the historical TUF target
against the embedded trusted root. Other legacy builds and newer theme schemas
require their own compatible implementation and stay in maintenance.

The adapter directory is immutable to PHP. After restoration starts, every PHP
role also loads the fixed HTTP and CLI guard. Ordinary pre-restore 3.1.0 operation
keeps its existing behavior. The adapter accepts only fixed phases and identities;
it exposes no script path or shell input and claims no coordinated v2 capability.

## Protocol floor and validation scope

`/var/lib/geoflow-updater/minimum-updater-protocol` persists independently of
operation status and business snapshots. The installer and runtime reject a
lower supported protocol even after an operation finishes. Application code
switch-back preserves the epoch and is blocked while restored work is held.

CI installs PHP with SQLite and an isolated Redis executable, runs the fixed PHP
adapter checks, and exercises Redis quarantine against a disposable Unix socket.
Unit and integration tests cover persistent boundaries, service ordering,
credential evidence, directory mounts, replay refusal, and restart behavior.
These checks do not replace an authorized disposable Docker host rehearsal of
full database restoration and ingress startup.

The legacy read surface is an audited allowlist. Unknown GET and HEAD handlers
are held because some legacy query pages create business records. Public page
rendering retains the active theme and public URLs through the pinned preview
context, suppresses article counters, and bypasses the public analytics writer.
Only the login, dashboard, public browsing, and explicitly listed read API routes
are available during this hold. Authentication and its session records remain
writable so that administrators can sign in again.

A reproducible application fixture is provided at
`internal/deployment/testdata/LegacyRecoveryAdapterIntegrationTest.php`. Copy it
into `tests/Feature/` of an isolated checkout at the pinned Core source commit,
install that checkout's test dependencies, set `GEOFLOW_LEGACY_ADAPTER_DIRECTORY`
to this repository's absolute `internal/deployment` path, and run the named PHP
feature test. Use a disposable SQLite test database. It migrates a fresh database
per test and checks credential invalidation, new login, rejected implicit GET
writes, and public rendering without analytics writes. The local evidence used
the pinned application source with the Core worktree's installed dependencies;
exact image and dependency validation still belongs to native host rehearsal.
