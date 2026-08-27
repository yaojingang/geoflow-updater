# Security policy

## Reporting a vulnerability

Use GitHub private vulnerability reporting for this repository. Include the affected updater and GEOFlow versions, deployment mode, reproduction steps, and any relevant logs with credentials removed.

Please avoid public issues for suspected signature bypasses, release-key exposure, control-token disclosure, backup leakage, or command-execution vulnerabilities. Maintainers will acknowledge a complete report, assess impact, and coordinate a fix and disclosure timeline.

## Release trust

Official updater archives are published through GitHub Releases and authorized by the embedded TUF root. The root role uses three Ed25519 keys with a threshold of two. Managed Compose, release manifests, checksums, and updater archives are TUF targets. The administrator bridge separately verifies a targets-role signature before exposing an installer download.

Reports about unsigned third-party builds should be sent to the distributor of that build.
