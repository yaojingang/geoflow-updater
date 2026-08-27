# GEOFlow Updater

GEOFlow Updater is the privileged host-side control plane for signed GEOFlow releases. It runs outside Laravel, owns the managed Docker Compose deployment, keeps the application away from the Docker socket, and exposes a small authenticated API over a Unix socket.

The current implementation provides:

- static Linux binaries for amd64 and arm64;
- `enroll` for registering an existing single-host Docker Compose deployment;
- `doctor` for machine-readable host and deployment diagnostics;
- a systemd service and local installer package;
- a go-tuf v2 client with an embedded two-of-three offline root of trust;
- digest-pinned GEOFlow application and web images;
- a signed bootstrap manifest for the GEOFlow administrator bridge;
- transaction stages for resolve, preflight, pull, quiesce, backup, migrate, activate, resume, and verify;
- PostgreSQL custom-format dumps, compressed site storage and persistent Redis data, deployment state, and configuration recovery points;
- automatic rollback after protected-stage failures and startup reconciliation after interrupted operations;
- authenticated typed operations for update, backup, verification, recovery-point listing, and rollback;
- administrator-held six-digit mutation authorization with replay protection for website-triggered update, backup, and rollback requests;
- direct CLI operations and administrator update-center controls.

The Laravel update executor is retired. Its database records remain available as read-only history in the administrator update center. Serialized legacy jobs are handled by a one-release tombstone that records retirement without changing files, databases, or containers.

## Supported environment

- Linux with systemd
- Docker Engine with Docker Compose v2
- Single host
- One managed instance named `primary`
- GEOFlow deployment with bundled PostgreSQL
- Existing deployment root containing `.env.prod` and `storage/`

Enrollment requires the installed `version.json` to match the signed managed release. The first handover therefore attaches the updater to an already matching release. Later releases use the transactional update path.

## Install and enroll

Download the release archive and `checksums.txt` from the [GitHub Releases page](https://github.com/yaojingang/geoflow-updater/releases). Verify its GitHub artifact attestation and checksum before extracting it. The administrator update center verifies the separately signed bootstrap manifest, digest, size, platform, and expiry when it prepares the package.

```bash
gh attestation verify geoflow-updater_VERSION_linux_ARCH.tar.gz \
  --repo yaojingang/geoflow-updater
sha256sum --check checksums.txt --ignore-missing
tar -xzf geoflow-updater_VERSION_linux_ARCH.tar.gz
sudo ./packaging/scripts/install.sh
sudo geoflow-updater enroll --instance-id primary --instance-root /opt/geoflow
sudo geoflow-updater authorization-uri --instance primary
sudo docker compose \
  --env-file /opt/geoflow/.env.prod \
  --env-file /var/lib/geoflow-updater/instances/primary/release.env \
  -f /var/lib/geoflow-updater/instances/primary/docker-compose.managed.yml \
  down --remove-orphans
sudo docker compose \
  --env-file /opt/geoflow/.env.prod \
  --env-file /var/lib/geoflow-updater/instances/primary/release.env \
  -f /var/lib/geoflow-updater/instances/primary/docker-compose.managed.yml \
  up -d --remove-orphans
sudo geoflow-updater doctor --instance primary
```

Before running `down`, stop new work and confirm every GEOFlow queue is idle. Jobs still held in the legacy Redis container can be lost when that container stops because the legacy deployment does not persist Redis data. The explicit `down` is the planned handover boundary. It stops the standard `geoflow-laravel-prod` project before the signed deployment attaches the same database and storage paths. Both environment files are required: `.env.prod` supplies the site's credentials and volume settings, while `release.env` supplies signed image digests and updater-owned paths.

Enrollment preserves PostgreSQL 16 or 18 and Redis 7 or 8 according to `PGVECTOR_IMAGE` and `REDIS_IMAGE` in `.env.prod`. It resolves a relative `POSTGRES_DATA_DIR` against the existing GEOFlow root, verifies the cluster's `PG_VERSION`, and writes the canonical source and container mount paths into updater-owned state. If either image reference has no recognizable major tag, set `GEOFLOW_UPDATER_POSTGRES_MAJOR` or `GEOFLOW_UPDATER_REDIS_MAJOR` explicitly before enrollment. Missing clusters, version mismatches, unsafe mount targets, and unsupported majors are rejected before managed state or containers change. `/opt/geoflow` is the recommended root; paths protected or isolated by the installed systemd sandbox are rejected during enrollment.

The installer does not support a remote `curl | sudo sh` flow. Review the extracted files before running the local script.

## Transactional operations

The administrator update center uses the authenticated Unix-socket API. `authorization-uri` creates three authenticator entries scoped to update, backup, and rollback. Each website mutation requires the instance control token and a fresh code from the matching entry. Accepted counters are persisted separately and consumed once. Five consecutive invalid guesses within one scope or across all scopes start a persistent 15-minute lockout; later invalid guesses double the delay up to 24 hours. A successful authorization clears the accepted scope while failures from other scopes remain in the aggregate anti-spray budget. Status, recovery-point listing, and verification remain control-token operations. Website rollback is fixed to the newest pre-update checkpoint. Root can select any verified recovery point through the host CLI:

```bash
sudo geoflow-updater update --instance primary
sudo geoflow-updater backup --instance primary
sudo geoflow-updater verify --instance primary
sudo geoflow-updater recovery-points --instance primary
sudo geoflow-updater rollback --instance primary --recovery-point RECOVERY_POINT_ID
```

An update resolves TUF metadata and signed targets, requires a higher release sequence, pulls immutable image digests, enters maintenance mode, and creates a recovery point before migrations or deployment activation. The updater restores the recovery point when migration, activation, startup, or verification fails. Restoring a Phase B release keeps its retired update worker stopped while the host updater remains the sole mutation authority. Operation state is written atomically after every stage. On service startup, an interrupted update is safely completed after verification or restored according to its last durable stage. Failed startup recovery is persisted as `recovery_required`; the socket remains available and retry attempts use bounded exponential backoff.

Recovery points include a PostgreSQL custom-format dump, the complete `storage/` tree, the stopped Redis data directory, `.env.prod`, `version.json`, and updater-owned deployment files. File ownership and permissions are preserved. Every artifact and directory archive is verified and staged before restoration mutates configuration or data. Nested mounts are rejected to keep external datasets outside backup and cleanup operations. Five recovery points are retained by default: one slot protects the newest pre-update checkpoint when present, and the remaining slots keep the newest points. A selected point receives a full integrity check before the deployment enters maintenance mode.

## Managed state

| Path | Purpose |
|---|---|
| `/usr/local/sbin/geoflow-updater` | Static updater binary |
| `/var/lib/geoflow-updater` | TUF cache and manager-owned instance state |
| `/run/geoflow-updater/geoflow-updater.sock` | Local control API socket |
| `/var/backups/geoflow-updater` | Root-only transactional recovery points |

Each enrolled instance receives `instance.yml`, `release.env`, `docker-compose.managed.yml`, a private `control.token`, and an optional root-only `mutation.secret` after authorization provisioning. Three operation-specific authenticator secrets are derived from that master secret. The application container receives the socket and its instance token. It does not receive `mutation.secret` or `/var/run/docker.sock`. The `doctor` command checks the installed version, database cluster, mutation authorization, strict release pins, effective Compose configuration, and the running or healthy state of every required managed container.

## Trust and releases

The embedded `root.json` authorizes three offline Ed25519 root keys with a threshold of two. Targets, snapshot, and timestamp use separate online keys. TUF protects the managed Compose file, GEOFlow `version.json`, release manifest, updater archives, and checksums. Image references in the release manifest use immutable OCI digests.

Release publishing runs in the protected `release-signing` GitHub environment. The workflow requires these base64-encoded PKCS#8 PEM secrets:

- `TUF_TARGETS_KEY_B64`
- `TUF_SNAPSHOT_KEY_B64`
- `TUF_TIMESTAMP_KEY_B64`

Daily availability refresh runs in a branch-restricted `metadata-refresh` environment with only `TUF_SNAPSHOT_KEY_B64` and `TUF_TIMESTAMP_KEY_B64`. It extends snapshot and timestamp metadata without loading the targets key. A release or the reviewer-gated `targets-refresh.yml` workflow must renew targets metadata before its 90-day expiry.

Keep the three root private keys offline and on separate custody devices. They are only needed for root rotation or emergency trust changes.

## Development

```bash
go test -race ./...
go vet ./...
docker compose -f assets/docker-compose.managed.yml config --quiet
```

The repository is licensed under Apache-2.0.

Security reports and release operations are documented in [SECURITY.md](SECURITY.md) and [docs/release-runbook.md](docs/release-runbook.md).
