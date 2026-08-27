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
6. Protect `main` and require the CI workflow before merge while allowing the metadata workflows to publish signed metadata.
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
9. Run enrollment and `doctor` on clean amd64 and arm64 Linux hosts using the matching GEOFlow bridge release.
10. Stop new work, confirm all GEOFlow queues are idle, and record the check before handover. The legacy Redis container has no persistent volume, so pending jobs can be lost when it stops.
11. On each architecture, exercise handover from the standard `geoflow-laravel-prod` project and confirm only one PostgreSQL container can attach the production data directory.

The publisher rejects a non-increasing release sequence, unrecognized manifest fields, mutable image tags, unofficial image repositories, malformed semantic versions, and a Compose target outside the fixed managed path.

## Failure handling

- If build, signing, or local TUF validation fails before GitHub Release creation, fix the cause and dispatch again with the same inputs.
- If GitHub Release creation succeeds and the metadata commit fails, keep the release out of installation instructions, inspect the branch movement, and complete the metadata publication only after confirming the generated digests. Use a new updater version when any release asset changes.
- If an online signing key is exposed, stop releases, rotate that role through a new root metadata version signed by two root custodians, publish the new root chain, and refresh all online metadata.
- If fewer than two root keys remain available, release trust cannot be changed. Restore a verified offline copy and complete a custody review before resuming publication.

Phase A enrollment only accepts a signed managed release whose version equals the installed GEOFlow `version.json`. Database-changing updates, transactional backups, verification, and rollback begin in Phase B.
