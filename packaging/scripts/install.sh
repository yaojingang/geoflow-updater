#!/usr/bin/env sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "Run this installer as root after downloading and verifying the release archive." >&2
    exit 1
fi

if [ "$(uname -s)" != "Linux" ]; then
    echo "GEOFlow Updater supports Linux hosts." >&2
    exit 1
fi

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
archive_root=$(CDPATH='' cd -- "$script_dir/../.." && pwd)

if ! command -v systemctl >/dev/null 2>&1; then
    echo "systemd is required." >&2
    exit 1
fi

if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
    echo "Docker Engine with Docker Compose v2 is required." >&2
    exit 1
fi

if ! "$archive_root/geoflow-updater" version >/dev/null 2>&1; then
    echo "The updater binary cannot run on this host architecture." >&2
    exit 1
fi

# The host floor survives completed operations and business snapshot restores.
floor_path=/var/lib/geoflow-updater/minimum-updater-protocol
if [ -e "$floor_path" ] || [ -d /var/lib/geoflow-updater/recovery-control ]; then
    if [ -L "$floor_path" ] || [ ! -f "$floor_path" ]; then
        echo "Unsafe updater protocol floor." >&2
        exit 1
    fi
    minimum_protocol=$(cat "$floor_path")
    candidate_protocol=$("$archive_root/geoflow-updater" protocol) || {
        echo "This updater cannot enforce the host recovery contract." >&2
        exit 1
    }
    case "$minimum_protocol" in
        ''|*[!0-9]*) echo "Invalid host updater protocol floor." >&2; exit 1 ;;
    esac
    case "$candidate_protocol" in
        ''|*[!0-9]*) echo "Invalid updater protocol identity." >&2; exit 1 ;;
    esac
    if [ "${#minimum_protocol}" -gt 9 ] || [ "${#candidate_protocol}" -gt 9 ] || [ "$minimum_protocol" -lt 5 ]; then
        echo "Unsupported updater protocol identity." >&2
        exit 1
    fi
    if [ "$candidate_protocol" -lt "$minimum_protocol" ]; then
        echo "This host requires a newer updater protocol; downgrade refused." >&2
        exit 1
    fi
fi

if ! getent group geoflow-updater >/dev/null 2>&1; then
    groupadd --system geoflow-updater
fi

install -o root -g root -m 0755 "$archive_root/geoflow-updater" /usr/local/sbin/geoflow-updater
install -o root -g root -m 0644 "$archive_root/packaging/systemd/geoflow-updater.service" /etc/systemd/system/geoflow-updater.service
install -d -o root -g geoflow-updater -m 0750 /var/lib/geoflow-updater
install -d -o root -g root -m 0750 /var/lib/geoflow-updater/instances
install -d -o root -g geoflow-updater -m 0750 /var/backups/geoflow-updater

systemctl daemon-reload
systemctl enable geoflow-updater.service
systemctl restart geoflow-updater.service

echo "GEOFlow Updater installed. Run: sudo geoflow-updater enroll --instance-root /absolute/path/to/geoflow"
