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
- PostgreSQL custom-format dumps, compressed site storage, deployment state, and configuration recovery points;
- automatic rollback after protected-stage failures and startup reconciliation after interrupted operations;
- authenticated typed operations for update, backup, verification, recovery-point listing, and rollback;
- direct CLI operations and administrator update-center controls.

The existing Laravel update executor remains available during the Phase B bridge period and is retired in Phase C after stability verification.

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
sudo docker compose \
  --env-file /opt/geoflow/.env.prod \
  --env-file /var/lib/geoflow-updater/instances/primary/release.env \
  -f /var/lib/geoflow-updater/instances/primary/docker-compose.managed.yml \
  down
sudo docker compose \
  --env-file /opt/geoflow/.env.prod \
  --env-file /var/lib/geoflow-updater/instances/primary/release.env \
  -f /var/lib/geoflow-updater/instances/primary/docker-compose.managed.yml \
  up -d
sudo geoflow-updater doctor --instance primary
```

Before running `down`, stop new work and confirm every GEOFlow queue is idle. Jobs still held in the legacy Redis container can be lost when that container stops because the legacy deployment does not persist Redis data. The explicit `down` is the planned handover boundary. It stops the standard `geoflow-laravel-prod` project before the signed deployment attaches the same database and storage paths. Both environment files are required: `.env.prod` supplies the site's credentials and volume settings, while `release.env` supplies signed image digests and updater-owned paths.

Enrollment preserves PostgreSQL 16 or 18 and Redis 7 or 8 according to `PGVECTOR_IMAGE` and `REDIS_IMAGE` in `.env.prod`. It resolves a relative `POSTGRES_DATA_DIR` against the existing GEOFlow root, verifies the cluster's `PG_VERSION`, and writes the canonical source and container mount paths into updater-owned state. If either image reference has no recognizable major tag, set `GEOFLOW_UPDATER_POSTGRES_MAJOR` or `GEOFLOW_UPDATER_REDIS_MAJOR` explicitly before enrollment. Missing clusters, version mismatches, unsafe mount targets, and unsupported majors are rejected before managed state or containers change.

The installer does not support a remote `curl | sudo sh` flow. Review the extracted files before running the local script.

## Transactional operations

The administrator update center uses the authenticated Unix-socket API. The same fixed operations are available on the host:

```bash
sudo geoflow-updater update --instance primary
sudo geoflow-updater backup --instance primary
sudo geoflow-updater verify --instance primary
sudo geoflow-updater recovery-points --instance primary
sudo geoflow-updater rollback --instance primary --recovery-point RECOVERY_POINT_ID
```

An update resolves TUF metadata and signed targets, requires a higher release sequence, pulls immutable image digests, enters maintenance mode, and creates a recovery point before migrations or deployment activation. The updater restores the recovery point when migration, activation, startup, or verification fails. Operation state is written atomically after every stage. On service startup, an interrupted update is either safely completed after verification or restored according to its last durable stage.

Recovery points include a PostgreSQL custom-format dump, the complete `storage/` tree with ownership and permissions, `.env.prod`, `version.json`, and updater-owned deployment files. Every artifact is verified against its manifest before restoration begins. The newest five valid recovery points are retained by default.

## Managed state

| Path | Purpose |
|---|---|
| `/usr/local/sbin/geoflow-updater` | Static updater binary |
| `/var/lib/geoflow-updater` | TUF cache and manager-owned instance state |
| `/run/geoflow-updater/geoflow-updater.sock` | Local control API socket |
| `/var/backups/geoflow-updater` | Root-only transactional recovery points |

Each enrolled instance receives `instance.yml`, `release.env`, `docker-compose.managed.yml`, and a private `control.token`. The application container receives the socket and its instance token. It never receives `/var/run/docker.sock`. The `doctor` command checks the installed version, database cluster, strict release pins, effective Compose configuration, and the running or healthy state of every required managed container.

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
