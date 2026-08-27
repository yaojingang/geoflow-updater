# Security policy

## Reporting a vulnerability

Use GitHub private vulnerability reporting for this repository. Include the affected updater and GEOFlow versions, deployment mode, reproduction steps, and any relevant logs with credentials removed.

Please avoid public issues for suspected signature bypasses, release-key exposure, control-token disclosure, backup leakage, or command-execution vulnerabilities. Maintainers will acknowledge a complete report, assess impact, and coordinate a fix and disclosure timeline.

## Release trust

Official updater archives are published through GitHub Releases and authorized by the embedded TUF root. The root role uses three Ed25519 keys with a threshold of two. Managed Compose, release manifests, checksums, and updater archives are TUF targets. The administrator bridge separately verifies a targets-role signature before exposing an installer download.

Reports about unsigned third-party builds should be sent to the distributor of that build.

## Local authorization boundary

The application container receives a per-instance control token for status, diagnostics, recovery-point listing, verification, and operation progress. Website-triggered update, backup, and rollback requests also require a six-digit TOTP code. The root-only master secret derives a distinct factor for each operation. The updater accepts a bounded clock window and persists per-operation consumed counters under an exclusive lock. Five consecutive invalid guesses trigger a persistent 15-minute lockout shared by all three factors. Further invalid guesses after expiry double the delay up to 24 hours. A valid authorization resets the failure state and can start one matching mutation request. Website rollback is bound to the newest pre-update checkpoint selected by the updater.

Direct root CLI operations remain available because root already controls the host. A compromised application can observe a code entered during a legitimate administrator request and may attempt to race the same scoped operation. Replay protection limits the code to one accepted use. Administrators should investigate unexpected rejection or operation activity and rotate all three derived factors by replacing the root-only master secret during a maintenance window.
