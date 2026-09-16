# Coordinated updater protocol v2

The local control credential protects `/v2/instances/primary/`. The v1 operation fields, status values and stage limit remain unchanged. New clients use v2 for all writes and query the original request after a lost response.

## Endpoints

| Method | Path after the instance prefix | Result |
| --- | --- | --- |
| GET | `capabilities` | Protocol 2, updater protocol 5, actions, features and recovery state |
| POST | `plans` | A 10-minute action plan with a host-generated ID and hash |
| GET | `plans/{id}` | The saved public plan |
| POST | `operations` | The original or newly admitted request receipt |
| GET | `requests/{id}` | The durable request receipt; exact absence is `404 request_not_found` |
| GET | `operations/{id}` | The corresponding request receipt |

The plan request contains `action`, `expected_epoch`, `actor`, and `recovery_point_id` only for restore. The actor contains the current management instance UUID, positive admin ID, and incarnation SHA-256. Core verifies the live account and permission; the host validates types and scope/action consistency.

Submission contains `client_request_id`, `actor`, `scope`, and exactly six business values: `action`, `plan_id`, `plan_sha256`, `expected_epoch`, `allow_maintenance`, `confirm_host_access`. The business SHA-256 hashes their alphabetically sorted compact JSON object. Both flags are JSON booleans. The six-digit OTP travels only in `X-GEOFlow-Updater-Authorization`. Passwords, OTPs and the control credential are never saved in plans or admissions.

## Authority and restart

Host-private files live in `STATE_DIR/coordination/primary/{plans,admissions}`, outside application snapshots and public recovery mounts. An admission binds the request hash, original actor, plan, epoch, authorization scope/matched counter, monotonic admission sequence and preallocated operation ID. Its v1 operation snapshot is updated durably before writing the existing operation/current projections. A private `coordination/primary/head.json` records the same monotonic order for host/v1 starts. For those starts it is an authoritative operation snapshot; for v2 it projects the admission. A newer explicit recovery therefore supersedes an older unresolved request without rewriting that request’s result, including after all operation projections are lost.

The lock order is `mutation.lock` then `operation.lock`. New submissions recheck the plan and current deployment under the operation lock. Every admission file and parent directory are synced before dispatch. Every write synchronizes all directory entries from the state-directory boundary, including existing entries left behind by a previously failed parent sync; a repeated sync failure keeps dispatch blocked. The authorization service takes the maximum of the legacy counter file and durable admission counters for both v1 and v2. Accepted v2 cleanup failures cannot release the consumed factor or erase the receipt.

A visible queued admission returns `admission_status=pending` and `operation=null`. It reserves the same request and operation IDs while projection or durability is unresolved. A restart first syncs the authoritative files’ directory entries and every parent from the state-directory boundary. It then rebuilds lost operation projections and reconciles the original operation. An action with no recorded stage ends as the existing v1 `failed` status without calling deployment code. Interrupted actions with stages use the existing conservative recovery rules. Queries never create a plan, repair files or dispatch work.

The same request ID and business hash returns the original receipt before checking a new OTP, plan expiry or current epoch. A different hash returns 409. A missing request receipt never authorizes automatic resubmission. Current Core credentials and permissions are still checked by Core before forwarding a lookup.

## Plans and recovery

All plans bind the current configuration, deployment artifacts and recovery epoch. Update also binds the inspected signed target and pins that target for execution. Restore binds the latest update checkpoint and its manifest. Switch-back binds the original completed deployment transaction and retained application files. A changed baseline requires a fresh plan.

Valid plans and unresolved admitted operations protect their recovery points from retention pruning. The latest update checkpoint is always retained. This implementation retains full admission receipts indefinitely, meeting the 90-day minimum and preserving the minimum mapping indefinitely. It performs no automatic receipt deletion. Storage errors reject new admissions or leave the original identity pending.

Target compatibility currently defaults to `continuation=host_only`: existing signed release metadata does not establish that the destination Core can return v2 receipts. The plan requires explicit host-access confirmation. Read a receipt from the host with:

```text
geoflow-updater request REQUEST_ID [--json]
```

The command performs no mutation and returns a nonzero exit status for failed, rolled-back, recovery-required or background-held results. A full restore continues to report `background_status=held`; protocol v2 does not release quarantine or transition a restored instance to `ready`.

## Verification harness

Normal Go tests cover the HTTP wire, real authorization service, concurrent duplicate admission, shared v1/v2 replay limits, failed cleanup, storage boundaries, missing projections, clock rollback, four plan baselines and recovery-point retention.

A separate, opt-in test-only Unix-socket fixture supports the real Core and CLI clients. Create an isolated directory under `/private/tmp` or `/tmp`, then run:

```text
GEOFLOW_COORDINATION_FIXTURE_DIR=/private/tmp/geoflow-coordination-fixture go test ./internal/agent -run '^TestCoordinatedSocketFixture$' -count=1 -timeout=16m
```

`fixture.json` supplies the temporary socket, state path, test control-credential path, epoch and current test-only backup OTP. `executions` counts the fake backup calls. Create `stop` in the fixture directory for graceful shutdown; remove it before restarting with the same directory. The fixture uses no Docker containers, business database or real backup. It is absent from the production binary.
