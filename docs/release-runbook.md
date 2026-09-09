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
6. Dispatch `planned-acceptance.yml` with the successful candidate run ID. The workflow uses fresh native amd64 and arm64 Linux VMs. Each architecture runs the container contract checks, a complete installed-host upgrade and restore, an interrupted first-install retry, and a separate online fixture test.
7. Review every architecture result and its operation, journal, HTTP, queue, and recovery evidence. The host tests cover durable-stage interruptions, failed-restoration backoff, post-traffic recovery policy, and restoration of PostgreSQL, Redis, storage, configuration, migration history, and deployment identity. See [planned host acceptance](planned-host-acceptance.md) for the exact scope.
8. Review the online fixture separately. It derives from the candidate application with no pending migrations and a signed plan limited to that candidate's sequence. It checks HTTP sessions, pending queue jobs, cross-slot Reverb delivery, reconnect, and application switch-back that preserves live data. Production online plans require source-to-target compatibility acceptance; the ordinary publication gate currently accepts maintenance plans only.
9. Download the `planned-acceptance-<run-id>` artifact and review `evidence-template.json`. Fill the release operator, security reviewer, and product owner decisions after reviewing the evidence. The workflow leaves those approvals pending.
10. Store the completed JSON as `PHASE_C_REHEARSAL_EVIDENCE_B64` in the protected environment. Dispatch `release.yml` with the candidate run ID and decoded evidence SHA-256. The gate rejects missing required cases, mixed candidate runs, emulated architectures, incomplete host modes, and missing approvals.
11. Confirm the updater commit, GEOFlow commit, target bytes, image digests, and archive attestations still match the candidate. Publication requires the same updater `main` commit used to build the candidate. Merge runtime fixes before building a replacement candidate and rerun acceptance. Harness-only descendants may debug an earlier candidate, but cannot substitute for final publication evidence.
12. Confirm the GitHub Release is public before Pages serves the new TUF timestamp. The publisher promotes the tested main candidate image digests and publishes `candidate/targets-source`. The online fixture remains outside that path.

The publisher requires a schema-3 release manifest and updater protocol 3 for maintenance plans or protocol 4 for online plans. It rejects non-increasing sequences, mutable image tags, unofficial image repositories, malformed versions, unapproved candidates, and evidence that does not exactly match the candidate. Every published release includes an attested `publication-authorization.json`.

The older `phase-c-rehearsal.yml` covers schema-2 releases. Use the planned acceptance workflow for schema-3 candidates. Site operators still need a maintenance window for the initial handover, an idle-queue check before stopping the legacy stack, and a verified backup suitable for their own data volume and retention requirements.

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
