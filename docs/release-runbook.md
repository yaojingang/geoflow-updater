# GEOFlow Updater release runbook

## Trust custody

Store each of the three root private keys on a separate encrypted offline device under separate custody. A root change requires two custodians. Keep targets, snapshot, and timestamp keys in the protected `release-signing` GitHub environment and retain an encrypted recovery copy outside GitHub.

Record the public root version, key IDs, custodians, creation date, expiry date, and recovery test date in the private release register. Public repositories contain signed metadata and public keys only.

## Metadata expiry policy

| Role | Validity | Renewal |
|---|---:|---|
| root | 730 days | Offline 2-of-3 ceremony before expiry or key rotation |
| targets | 90 days | Each release or reviewer-gated manual refresh |
| snapshot | 30 days | Daily automated online refresh |
| timestamp | 7 days | Daily online metadata refresh |

The scheduled `metadata-refresh.yml` workflow renews snapshot and timestamp metadata every day without access to the targets key. Alert when the workflow has not completed for 48 hours, timestamp validity falls below 72 hours, or targets validity falls below 30 days. Dispatch `targets-refresh.yml` through reviewer approval when no product release will renew targets in time.

## First-time repository setup

1. Create the public repository with Apache-2.0 licensing.
2. Enable GitHub Actions as the Pages source.
3. Create the protected `release-signing` environment with required reviewers and add `TUF_TARGETS_KEY_B64`, `TUF_SNAPSHOT_KEY_B64`, `TUF_TIMESTAMP_KEY_B64`, and the per-candidate `PHASE_C_REHEARSAL_EVIDENCE_B64` after rehearsal approval.
4. Create the branch-restricted `metadata-refresh` environment without required reviewers and add only `TUF_SNAPSHOT_KEY_B64` and `TUF_TIMESTAMP_KEY_B64`.
5. Enable private vulnerability reporting.
6. Protect `main` from deletion and non-fast-forward updates, and require linear history. A strict pull-request and CI gate also needs a dedicated release GitHub App installed on the repository and configured as the signing-workflow bypass actor. Personal repositories cannot assign the built-in GitHub Actions App as that bypass actor.
7. Run the online metadata refresh workflow and verify the Pages metadata URL before the first updater release.

## Publishing a release

1. Confirm the GEOFlow ref is an approved immutable commit or protected tag and its `version.json` matches the release version.
2. Choose an updater semantic version that has never been published.
3. Choose a GEOFlow release sequence greater than the sequence in the currently signed release manifest.
4. Review the pinned PostgreSQL 16/18 and Redis 7/8 index digests in `release-candidate.yml`, refresh them when upstream security updates are approved, and verify each index includes `linux/amd64` and `linux/arm64`.
5. Dispatch `release-candidate.yml` through the protected `release-signing` environment. Record its run ID and download the immutable candidate artifact.
6. Dispatch `phase-c-rehearsal.yml` with the successful candidate run ID. It runs enrollment, `authorization-uri`, update, backup, rollback, failure recovery, restart reconciliation, and `doctor` on fresh native amd64 and arm64 GitHub-hosted Linux VMs against the exact private signed candidate repository.
7. Stop new work, confirm all GEOFlow queues are idle, and record the check before handover. For a Phase C cutover, also confirm `system_update_runs` contains no `queued` or `running` apply/rollback rows and the `system-updates` queue contains no pending, reserved, or delayed job. The legacy Redis container has no persistent volume, so queued work can be lost when it stops.
8. On each architecture, exercise handover from the standard `geoflow-laravel-prod` project and confirm only one PostgreSQL container can attach the production data directory. Confirm the retired `geoflow-system-update-queue-prod` container is absent after activation.
9. From the administrator update center, use distinct authenticator codes for a full backup, a transactional update that includes a migration, and rollback. Verify that reusing an accepted code fails and status/verification remain available.
10. Verify the recovery point, operation stages, new signed version document, healthy containers, and read-only legacy history on both architectures.
11. Review the workflow records for its forced migration and activation failures. Confirm that each scenario changed the protected surfaces after backup, restored PostgreSQL data and migration fingerprint, Redis, storage, site configuration, managed instance/release/Compose files, and Phase B version, removed the retired worker, passed `doctor`, and stayed final across a service restart.
12. Review the workflow records for its service termination during migration and resume, then verify startup reconciliation follows the last durable stage without exposing two active operations. Confirm the persisted recovery failure observes backoff and converges after the injected fault is removed.
13. Complete every evidence field in [the Phase C staging rehearsal](phase-c-staging-rehearsal.md). A release remains blocked until both architecture rows are approved.
14. Store the completed evidence JSON as `PHASE_C_REHEARSAL_EVIDENCE_B64` in the protected environment. Dispatch `release.yml` with the candidate run ID and decoded evidence SHA-256.
15. Confirm `release.yml` accepts the original archive attestations and hashes, source commits, target bytes, image digests, architecture results, and approvals. It republishes the candidate target bytes into current TUF metadata and promotes the already-tested image digests.
16. Verify the GitHub Release is public before Pages serves the new TUF timestamp. Confirm the signed manifest references `releases/<version>/version.json` and matches the rehearsed candidate hash.

The publisher rejects a non-increasing release sequence, a Phase C manifest without updater protocol 2, unrecognized fields, mutable image tags, unofficial image repositories, malformed semantic versions, a Compose target outside the fixed managed path, an unapproved candidate, and evidence that does not exactly match the candidate. Every published release includes `publication-authorization.json`, bound to the exact candidate and protected with a GitHub artifact attestation.

## Emergency super-administrator risk waiver

The repository owner may explicitly accept the missing dual-architecture host rehearsal when an urgent first release cannot obtain both disposable hosts. This path preserves the protected `release-signing` environment and every candidate, source, archive hash, artifact attestation, target byte, image digest, architecture-index, bootstrap, and TUF signature check.

Set `superadmin_risk_waiver` to `true`, leave `phase_c_evidence_sha256` empty, enter `I_ACCEPT_PHASE_C_RELEASE_WITHOUT_DUAL_ARCH_REHEARSAL`, and provide a 20 to 500 character reason. The initial workflow actor and any rerun actor must both equal the repository owner. Publication records the actor, reason, fixed acknowledgement, accepted architecture risks, and exact candidate in `publication-authorization.json`; the file is attached to the public GitHub Release and receives a GitHub artifact attestation.

Use the waiver for a specifically authorized release. Schedule amd64 and arm64 host rehearsals after publication, record any findings, and issue a new release sequence when remediation changes signed release content.

## Failure handling

- The candidate workflow builds images under run-specific candidate tags, signs an isolated TUF repository, and uploads an immutable artifact. It does not change public TUF metadata, public releases, version tags, or Pages.
- The publication workflow keeps candidate updater assets in a draft release until signed metadata is committed. A failure before the metadata commit can be retried with the same candidate and evidence; draft assets are replaced with the same verified bytes.
- If the metadata commit succeeds and a later publication step fails, dispatch the workflow again with the same candidate and evidence. It recognizes the signed release sequence, validates the existing target hash and release assets, promotes only matching image digests, and dispatches Pages without signing a second release.
- Immutable TUF metadata writers skip version numbers left by interrupted publications, so a failed metadata push can be retried after the branch conflict or service failure is resolved.
- If an online signing key is exposed, stop releases, rotate that role through a new root metadata version signed by two root custodians, publish the new root chain, and refresh all online metadata.
- If fewer than two root keys remain available, release trust cannot be changed. Restore a verified offline copy and complete a custody review before resuming publication.

Enrollment accepts a signed managed release whose version equals the installed GEOFlow `version.json`. Phase C uses a signed version document, administrator-held mutation authorization, a durable recovery point, automatic verification, and rollback before the operation reaches a terminal state.
