#!/usr/bin/env bash
set -Eeuo pipefail

real_docker=/usr/bin/docker
fault_file=/var/lib/geoflow-updater/rehearsal-fault
marker_dir=/var/lib/geoflow-updater/rehearsal-markers
candidate_compose=/var/lib/geoflow-updater/instances/primary/transaction/docker-compose.candidate.yml
fixture_file=/var/lib/geoflow-updater/rehearsal-restore-fixture
corruption_marker=/var/lib/geoflow-updater/rehearsal-corruption-applied
instance_root=/opt/geoflow-phase-c-rehearsal
managed_root=/var/lib/geoflow-updater/instances/primary

corrupt_recovery_surfaces() {
    local fixture_id
    fixture_id=$(tr -d '\n' < "$fixture_file")
    if [[ ! $fixture_id =~ ^phase-c-[a-z0-9-]+$ ]]; then
        echo "Phase C restore fixture is invalid." >&2
        exit 96
    fi
    "$real_docker" exec geoflow-postgres-prod psql --username=geo_user --dbname=geo_flow --set ON_ERROR_STOP=1 \
        --command "UPDATE system_update_runs SET target_version='corrupted' WHERE run_uuid='${fixture_id}'" >/dev/null
    "$real_docker" start geoflow-redis-prod >/dev/null
    "$real_docker" exec geoflow-redis-prod sh -eu -c \
        'redis-cli --no-auth-warning -a "$REDIS_PASSWORD" SET "geoflow:${1}:fault-restore" corrupted >/dev/null' \
        phase-c "$fixture_id"
    "$real_docker" stop --time 30 geoflow-redis-prod >/dev/null
    printf 'corrupted\n' > "$instance_root/storage/app/phase-c-fault-restore.txt"
    sed -i 's/^PHASE_C_FAULT_RESTORE_MARKER=.*/PHASE_C_FAULT_RESTORE_MARKER=corrupted/' "$instance_root/.env.prod"
    grep -q '^version:' "$managed_root/instance.yml"
    sed -i 's/^version:.*/version: corrupted/' "$managed_root/instance.yml"
    printf '# phase-c-corrupted\n' >> "$managed_root/release.env"
    printf '# phase-c-corrupted\n' >> "$managed_root/docker-compose.managed.yml"
    printf '%s\n' "$fixture_id" > "$corruption_marker"
}

fault=""
if [[ -f $fault_file ]]; then
    fault=$(tr -d '\n' < "$fault_file")
fi
arguments=" $* "

case "$fault" in
    fail-migrate)
        if [[ $arguments == *" run --rm --no-deps init php artisan migrate --force "* ]]; then
            corrupt_recovery_surfaces
            echo "Phase C rehearsal injected a migration command failure." >&2
            exit 97
        fi
        ;;
    fail-activate)
        if [[ $arguments == *" run --rm --no-deps init php artisan migrate --force "* ]]; then
            "$real_docker" "$@"
            corrupt_recovery_surfaces
            rm -f "$candidate_compose"
            echo "Phase C rehearsal removed the prepared Compose file after migration." >&2
            exit 0
        fi
        ;;
    pause-migrate)
        if [[ $arguments == *" run --rm --no-deps init php artisan migrate --force "* ]]; then
            "$real_docker" "$@"
            corrupt_recovery_surfaces
            install -d -o root -g root -m 0700 "$marker_dir"
            touch "$marker_dir/migrate-completed"
            while [[ -f $fault_file && $(tr -d '\n' < "$fault_file") == pause-migrate ]]; do
                sleep 1
            done
            exit 0
        fi
        ;;
    pause-migrate-fail-restore)
        if [[ $arguments == *" run --rm --no-deps init php artisan migrate --force "* ]]; then
            "$real_docker" "$@"
            corrupt_recovery_surfaces
            install -d -o root -g root -m 0700 "$marker_dir"
            touch "$marker_dir/migrate-completed"
            while [[ -f $fault_file && $(tr -d '\n' < "$fault_file") == pause-migrate-fail-restore ]]; do
                sleep 1
            done
            exit 0
        elif [[ $arguments == *" pg_restore "* ]]; then
            echo "Phase C rehearsal injected a recovery command failure." >&2
            exit 98
        fi
        ;;
    pause-resume)
        if [[ $arguments == *" run --rm --no-deps init php artisan up "* ]]; then
            "$real_docker" "$@"
            install -d -o root -g root -m 0700 "$marker_dir"
            touch "$marker_dir/resume-completed"
            while [[ -f $fault_file && $(tr -d '\n' < "$fault_file") == pause-resume ]]; do
                sleep 1
            done
            exit 0
        fi
        ;;
    fail-restore)
        if [[ $arguments == *" pg_restore "* ]]; then
            echo "Phase C rehearsal injected a recovery command failure." >&2
            exit 98
        fi
        ;;
    "")
        ;;
    *)
        echo "Unknown Phase C rehearsal fault mode." >&2
        exit 99
        ;;
esac

exec "$real_docker" "$@"
