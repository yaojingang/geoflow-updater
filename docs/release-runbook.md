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
3. Create the protected `release-signing` environment with required reviewers and add `TUF_TARGETS_KEY_B64`, `TUF_SNAPSHOT_KEY_B64`, and `TUF_TIMESTAMP_KEY_B64`.
4. Create the branch-restricted `metadata-refresh` environment without required reviewers and add only `TUF_SNAPSHOT_KEY_B64` and `TUF_TIMESTAMP_KEY_B64`.
5. Enable private vulnerability reporting.
6. Protect `main` from deletion and non-fast-forward updates, and require linear history. A strict pull-request and CI gate also needs a dedicated release GitHub App installed on the repository and configured as the signing-workflow bypass actor. Personal repositories cannot assign the built-in GitHub Actions App as that bypass actor.
7. Run the online metadata refresh workflow and verify the Pages metadata URL before the first updater release.

## Publishing a release

1. Confirm the GEOFlow ref is an approved immutable commit or protected tag and its `version.json` matches the release version.
2. Choose an updater semantic version that has never been published.
3. Choose a GEOFlow release sequence greater than the sequence in the currently signed release manifest.
4. Review the pinned PostgreSQL 16/18 and Redis 7/8 index digests in `release.yml`, refresh them when upstream security updates are approved, and verify each index includes `linux/amd64` and `linux/arm64`.
5. Dispatch `release.yml` through the protected `release-signing` environment.
6. Verify both multi-architecture GEOFlow image indexes contain `linux/amd64` and `linux/arm64`.
7. Verify GitHub artifact attestations and archive checksums.
8. Verify the GitHub Release is public before the new TUF timestamp is served by Pages.
9. Confirm the signed release manifest references `releases/<version>/version.json` and that the target matches the source ref.
10. Run enrollment and `doctor` on clean amd64 and arm64 Linux hosts using the matching GEOFlow bridge release.
11. Stop new work, confirm all GEOFlow queues are idle, and record the check before handover. The legacy Redis container has no persistent volume, so pending jobs can be lost when it stops.
12. On each architecture, exercise handover from the standard `geoflow-laravel-prod` project and confirm only one PostgreSQL container can attach the production data directory.
13. From the managed release, run a transactional update that includes a migration and verify the recovery point, operation stages, new signed version document, and healthy containers.
14. Force a protected-stage failure on a disposable host and verify automatic PostgreSQL, Redis, storage, configuration, and deployment rollback.
15. Restart the updater during migration and during resume, then verify startup reconciliation follows the last durable stage without exposing two active operations.

The publisher rejects a non-increasing release sequence, unrecognized manifest fields, mutable image tags, unofficial image repositories, malformed semantic versions, and a Compose target outside the fixed managed path.

## Failure handling

- The workflow builds images under run-specific staging tags and keeps updater assets in a draft release until signed metadata is committed. A failure before the metadata commit can be retried with the same inputs; draft assets are replaced by the retry.
- If the metadata commit succeeds and a later publication step fails, dispatch the workflow again with the same inputs. It recognizes the signed release sequence, validates the existing release assets, promotes only matching image digests, and dispatches Pages without signing a second release.
- Immutable TUF metadata writers skip version numbers left by interrupted publications, so a failed metadata push can be retried after the branch conflict or service failure is resolved.
- If an online signing key is exposed, stop releases, rotate that role through a new root metadata version signed by two root custodians, publish the new root chain, and refresh all online metadata.
- If fewer than two root keys remain available, release trust cannot be changed. Restore a verified offline copy and complete a custody review before resuming publication.

Enrollment accepts a signed managed release whose version equals the installed GEOFlow `version.json`. Phase B updates use a signed version document, a durable recovery point, automatic verification, and rollback before the operation reaches a terminal state.
