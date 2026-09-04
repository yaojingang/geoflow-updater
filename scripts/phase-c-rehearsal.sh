#!/usr/bin/env bash
set -Eeuo pipefail

umask 077

candidate_dir=""
phase_b_root=""
evidence_dir=""
while (($# > 0)); do
    case "$1" in
        --candidate-dir)
            candidate_dir=${2:-}
            shift 2
            ;;
        --phase-b-root)
            phase_b_root=${2:-}
            shift 2
            ;;
        --evidence-dir)
            evidence_dir=${2:-}
            shift 2
            ;;
        *)
            echo "Unknown argument: $1" >&2
            exit 2
            ;;
    esac
done

if [[ $EUID -ne 0 || -z $candidate_dir || -z $phase_b_root || -z $evidence_dir ]]; then
    echo "Run as root with candidate, Phase B, and evidence directories." >&2
    exit 2
fi
evidence_owner_uid=${SUDO_UID:-}
evidence_owner_gid=${SUDO_GID:-}
if [[ ! $evidence_owner_uid =~ ^[1-9][0-9]*$ || ! $evidence_owner_gid =~ ^[1-9][0-9]*$ ]]; then
    echo "Run through sudo so evidence can be returned to the invoking runner." >&2
    exit 2
fi
updater_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
for directory in "$candidate_dir" "$phase_b_root"; do
    if [[ $directory != /* || ! -d $directory || -L $directory ]]; then
        echo "A required source directory is unavailable or unsafe." >&2
        exit 2
    fi
done
expected_evidence_parent=$updater_root/rehearsal/evidence
expected_evidence_dir=$expected_evidence_parent/${PLATFORM:-unknown}
if [[ ! ${PLATFORM:-} =~ ^linux-(amd64|arm64)$ || $evidence_dir != "$expected_evidence_dir" || -e $evidence_dir || ! -d $expected_evidence_parent || -L $expected_evidence_parent ]]; then
    echo "The evidence directory is unsafe." >&2
    exit 2
fi
if [[ $(realpath -e "$expected_evidence_parent") != "$expected_evidence_parent" ]]; then
    echo "The evidence parent resolves outside the runner workspace." >&2
    exit 2
fi
if [[ $(stat -c '%u:%g' "$expected_evidence_parent") != "$evidence_owner_uid:$evidence_owner_gid" ]]; then
    echo "The evidence parent is not owned by the invoking runner." >&2
    exit 2
fi
mkdir "$evidence_dir"
evidence_marker=$evidence_dir/.phase-c-rehearsal-owned
: > "$evidence_marker"
instance_root=/opt/geoflow-phase-c-rehearsal
unsafe_root=/tmp/geoflow-phase-c-rehearsal-unsafe
state_dir=/var/lib/geoflow-updater
instance_dir=$state_dir/instances/primary
backup_root=/var/backups/geoflow-updater/primary
runtime_socket=/run/geoflow-updater/geoflow-updater.sock
repository_port=18443
repository_pid=""
website_cookie_jar=""
regular_cookie_jar=""
anonymous_cookie_jar=""
fault_file=$state_dir/rehearsal-fault
fault_marker_dir=$state_dir/rehearsal-markers
result_status=fail
current_check=bootstrap
checks_file=$evidence_dir/checks.jsonl
chmod 0700 "$evidence_dir"
: > "$checks_file"

declare -a sensitive_values=()
declare -A factor_secrets=()
declare -A last_totp_counters=()

log() {
    printf '[phase-c] %s\n' "$1"
}

record_check() {
    local check_id=$1
    local evidence=$2
    jq -cn --arg id "$check_id" --arg evidence "$evidence" \
        '{id:$id,status:"pass",evidence:$evidence}' >> "$checks_file"
}

collect_diagnostics() {
    systemctl status geoflow-updater.service --no-pager > "$evidence_dir/updater-service.txt" 2>&1 || true
    docker ps -a --format '{{json .}}' > "$evidence_dir/docker-containers.jsonl" 2>/dev/null || true
    if docker inspect --type=container geoflow-web-prod >/dev/null 2>&1; then
        docker inspect --format '{{json .State.Health}}' geoflow-web-prod > "$evidence_dir/web-health.json" 2>/dev/null || true
        docker logs geoflow-web-prod > "$evidence_dir/web-container.log" 2>&1 || true
        docker exec geoflow-web-prod nginx -T > "$evidence_dir/web-nginx-config.txt" 2>&1 || true
        docker exec geoflow-web-prod sh -lc \
            'wget -S -O - --header="Host: ${GEOFLOW_NGINX_PRIMARY_HOST:-localhost}" http://127.0.0.1/up' \
            > "$evidence_dir/web-health-probe.txt" 2>&1 || true
    fi
    uname -a > "$evidence_dir/kernel.txt" 2>&1 || true
    docker version > "$evidence_dir/docker-version.txt" 2>&1 || true
    docker compose version > "$evidence_dir/compose-version.txt" 2>&1 || true
}

write_result() {
    local failed_check=""
    if [[ $result_status != pass ]]; then
        failed_check=$current_check
    fi
    jq -s \
        --arg platform "${PLATFORM:-unknown}" \
        --arg archive_arch "${ARCHIVE_ARCH:-unknown}" \
        --arg kernel_arch "$(uname -m)" \
        --arg candidate_run_id "${CANDIDATE_RUN_ID:-unknown}" \
        --arg result "$result_status" \
        --arg failed_check "$failed_check" \
        '{schema_version:1,platform:$platform,archive_arch:$archive_arch,kernel_arch:$kernel_arch,candidate_run_id:$candidate_run_id,result:$result,failed_check:$failed_check,checks:.}' \
        "$checks_file" > "$evidence_dir/result.json"
}

handoff_evidence() {
    test -f "$evidence_marker"
    find "$evidence_dir" -type d -exec chmod 0700 {} +
    find "$evidence_dir" -type f -exec chmod 0600 {} +
    chown -R "$evidence_owner_uid:$evidence_owner_gid" "$evidence_dir"
}

redact_evidence() {
    local patterns_file
    patterns_file=$(mktemp /var/tmp/geoflow-phase-c-sensitive.XXXXXX)
    printf '%s\n' "${sensitive_values[@]}" > "$patterns_file"
    chmod 0600 "$patterns_file"
    PHASE_C_EVIDENCE_DIR=$evidence_dir PHASE_C_PATTERNS_FILE=$patterns_file python3 - <<'PY'
import os
import pathlib

root = pathlib.Path(os.environ["PHASE_C_EVIDENCE_DIR"])
patterns = [
    value
    for value in pathlib.Path(os.environ["PHASE_C_PATTERNS_FILE"]).read_bytes().splitlines()
    if value
]
for path in root.rglob("*"):
    if not path.is_file() or path.stat().st_size > 4 * 1024 * 1024:
        continue
    contents = path.read_bytes()
    redacted = contents
    for pattern in patterns:
        redacted = redacted.replace(pattern, b"[REDACTED]")
    if redacted != contents:
        path.write_bytes(redacted)
    if any(pattern in redacted for pattern in patterns):
        raise SystemExit(f"evidence redaction failed for {path}")
PY
    local redaction_status=$?
    rm -f "$patterns_file"
    return "$redaction_status"
}

finish() {
    local exit_code=$?
    set +e
    collect_diagnostics
    if [[ -n $repository_pid ]]; then
        kill "$repository_pid" >/dev/null 2>&1 || true
        wait "$repository_pid" >/dev/null 2>&1 || true
    fi
    if [[ -n $website_cookie_jar ]]; then
        rm -f "$website_cookie_jar"
    fi
    if [[ -n $regular_cookie_jar ]]; then
        rm -f "$regular_cookie_jar"
    fi
    if [[ -n $anonymous_cookie_jar ]]; then
        rm -f "$anonymous_cookie_jar"
    fi
    write_result || exit_code=1
    redact_evidence || exit_code=1
    handoff_evidence || exit_code=1
    trap - EXIT
    exit "$exit_code"
}
trap finish EXIT

require_command() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "Required command is unavailable: $1" >&2
        return 1
    }
}

mask_value() {
    local value=$1
    if [[ -n $value ]]; then
        printf '::add-mask::%s\n' "$value"
        sensitive_values+=("$value")
    fi
}

scan_runtime_logs() {
    local combined_logs
    combined_logs=$(mktemp /var/tmp/geoflow-phase-c-logs.XXXXXX)
    journalctl -u geoflow-updater.service --output=cat --lines=10000 > "$combined_logs" 2>/dev/null || true
    local container_name
    while IFS= read -r container_name; do
        docker logs "$container_name" 2>&1 | tail -c 4194304 >> "$combined_logs" || true
    done < <(docker ps -a --format '{{.Names}}')
    if [[ -d $instance_root/storage/logs ]]; then
        while IFS= read -r -d '' log_file; do
            tail -c 4194304 "$log_file" >> "$combined_logs" || true
        done < <(find "$instance_root/storage/logs" -type f -print0)
    fi
    local sensitive
    for sensitive in "${sensitive_values[@]}"; do
        if grep -Fq -- "$sensitive" "$combined_logs"; then
            rm -f "$combined_logs"
            echo "A sensitive rehearsal value appeared in runtime logs." >&2
            return 1
        fi
    done
    rm -f "$combined_logs"
}

read_environment_value() {
    local key=$1
    awk -v expected="$key" '
        index($0, expected "=") == 1 { value=substr($0, length(expected)+2) }
        END {
            if (value ~ /^".*"$/) value=substr(value, 2, length(value)-2)
            print value
        }
    ' "$instance_root/.env.prod"
}

api_status=""
api_body=""
api_request() {
    local method=$1
    local endpoint=$2
    local authorization_code=${3:-}
    local payload=${4:-}
    local response_file
    response_file=$(mktemp)
    local -a arguments=(
        --silent --show-error
        --unix-socket "$runtime_socket"
        --request "$method"
        --header "Authorization: Bearer $control_token"
        --output "$response_file"
        --write-out '%{http_code}'
    )
    if [[ -n $authorization_code ]]; then
        arguments+=(--header "X-GEOFlow-Updater-Authorization: $authorization_code")
    fi
    if [[ -n $payload ]]; then
        arguments+=(--header 'Content-Type: application/json' --data "$payload")
    elif [[ $method == POST ]]; then
        arguments+=(--header 'Content-Length: 0')
    fi
    local curl_status=0
    api_status=$(curl "${arguments[@]}" "http://localhost/v1/instances/primary/$endpoint") || curl_status=$?
    api_body=$(<"$response_file")
    rm -f "$response_file"
    return "$curl_status"
}

expect_api_error() {
    local method=$1
    local endpoint=$2
    local authorization_code=$3
    local payload=$4
    local expected_status=$5
    local expected_error=$6
    api_request "$method" "$endpoint" "$authorization_code" "$payload"
    test "$api_status" = "$expected_status"
    test "$(jq -er '.error' <<< "$api_body")" = "$expected_error"
}

operation_id=""
operation_recovery_point=""
start_mutation() {
    local endpoint=$1
    local authorization_code=$2
    local payload=${3:-}
    api_request POST "$endpoint" "$authorization_code" "$payload"
    test "$api_status" = 202
    operation_id=$(jq -er '.id' <<< "$api_body")
}

wait_for_operation() {
    local label=$1
    local expected_status=$2
    local expected_kind=$3
    local observed_status=""
    local completed_at=""
    for _ in $(seq 1 1800); do
        if ! api_request GET operations/current "" ""; then
            sleep 2
            continue
        fi
        test "$api_status" = 200
        test "$(jq -er '.id' <<< "$api_body")" = "$operation_id"
        test "$(jq -er '.kind' <<< "$api_body")" = "$expected_kind"
        observed_status=$(jq -er '.status' <<< "$api_body")
        completed_at=$(jq -r '.completed_at // empty' <<< "$api_body")
        if [[ -n $completed_at && $observed_status != recovery_required ]]; then
            printf '%s\n' "$api_body" | jq . > "$evidence_dir/operation-$label.json"
            test "$observed_status" = "$expected_status"
            operation_recovery_point=$(jq -r '.recovery_point_id // empty' <<< "$api_body")
            scan_runtime_logs
            return 0
        fi
        sleep 2
    done
    echo "Operation $label did not finish within 60 minutes." >&2
    return 1
}

wait_for_operation_status() {
    local expected_status=$1
    for _ in $(seq 1 300); do
        if api_request GET operations/current "" "" && [[ $api_status == 200 ]]; then
            test "$(jq -er '.id' <<< "$api_body")" = "$operation_id"
            if [[ $(jq -er '.status' <<< "$api_body") == "$expected_status" ]]; then
                printf '%s\n' "$api_body" | jq . > "$evidence_dir/operation-recovery-required.json"
                scan_runtime_logs
                return 0
            fi
        fi
        sleep 1
    done
    echo "Operation did not reach $expected_status." >&2
    return 1
}

wait_for_service() {
    for _ in $(seq 1 90); do
        if systemctl is-active --quiet geoflow-updater.service && [[ -S $runtime_socket ]]; then
            return 0
        fi
        sleep 1
    done
    echo "Updater service did not return after restart." >&2
    return 1
}

set_fault() {
    local mode=$1
    install -d -o root -g root -m 0700 "$fault_marker_dir"
    rm -f "$fault_marker_dir/migrate-completed" "$fault_marker_dir/resume-completed"
    printf '%s\n' "$mode" > "$fault_file"
    chmod 0600 "$fault_file"
}

clear_fault() {
    rm -f "$fault_file"
}

wait_for_fault_marker() {
    local marker_name=$1
    for _ in $(seq 1 600); do
        if [[ -f $fault_marker_dir/$marker_name ]]; then
            return 0
        fi
        sleep 1
    done
    echo "Fault marker $marker_name was not reached." >&2
    return 1
}

generate_totp_for_secret() {
    local secret=$1
    local counter=$2
    TOTP_SECRET=$secret TOTP_COUNTER=$counter python3 - <<'PY'
import base64
import hashlib
import hmac
import os
import struct

secret = base64.b32decode(os.environ["TOTP_SECRET"])
counter = int(os.environ["TOTP_COUNTER"])
digest = hmac.new(secret, struct.pack(">Q", counter), hashlib.sha1).digest()
offset = digest[-1] & 0x0F
value = struct.unpack(">I", digest[offset:offset + 4])[0] & 0x7FFFFFFF
print(f"{value % 1000000:06d}")
PY
}

totp_code=""
next_totp() {
    local scope=$1
    local counter
    while true; do
        counter=$(( $(date +%s) / 30 ))
        if (( counter > ${last_totp_counters[$scope]:--1} )); then
            break
        fi
        sleep 1
    done
    totp_code=$(generate_totp_for_secret "${factor_secrets[$scope]}" "$counter")
    last_totp_counters[$scope]=$counter
    mask_value "$totp_code"
}

website_base_url=http://127.0.0.1:18080/geo_admin
website_status=""
website_location=""

extract_csrf_token() {
    local html_file=$1
    PHASE_C_HTML_FILE=$html_file python3 - <<'PY'
from html.parser import HTMLParser
import os

class TokenParser(HTMLParser):
    def __init__(self):
        super().__init__()
        self.token = ""

    def handle_starttag(self, tag, attrs):
        values = dict(attrs)
        if tag == "input" and values.get("name") == "_token":
            self.token = values.get("value", "")

parser = TokenParser()
with open(os.environ["PHASE_C_HTML_FILE"], encoding="utf-8") as source:
    parser.feed(source.read())
if not parser.token:
    raise SystemExit("CSRF token is missing")
print(parser.token)
PY
}

website_get() {
    local cookie_jar=$1
    local path=$2
    local output=$3
    website_status=$(curl --silent --show-error \
        --connect-timeout 5 --max-time 30 \
        --header 'Host: localhost' \
        --cookie "$cookie_jar" --cookie-jar "$cookie_jar" \
        --output "$output" --write-out '%{http_code}' \
        "$website_base_url/$path")
}

website_post() {
    local cookie_jar=$1
    local path=$2
    local csrf_token=$3
    local output=$4
    shift 4
    local headers
    headers=$(mktemp /var/tmp/geoflow-phase-c-headers.XXXXXX)
    local -a arguments=(
        --silent --show-error
        --connect-timeout 5 --max-time 30
        --header 'Host: localhost'
        --cookie "$cookie_jar" --cookie-jar "$cookie_jar"
        --dump-header "$headers"
        --output "$output" --write-out '%{http_code}'
        --request POST
    )
    if [[ -n $csrf_token ]]; then
        arguments+=(--data-urlencode "_token=$csrf_token")
    fi
    local field
    for field in "$@"; do
        arguments+=(--data-urlencode "$field")
    done
    website_status=$(curl "${arguments[@]}" "$website_base_url/$path")
    website_location=$(awk 'BEGIN { IGNORECASE=1 } /^location:/ { sub(/^[^:]+:[[:space:]]*/, ""); sub(/\r$/, ""); print; exit }' "$headers")
    rm -f "$headers"
}

mask_cookie_jar() {
    local cookie_jar=$1
    local cookie_value
    while IFS= read -r cookie_value; do
        mask_value "$cookie_value"
    done < <(awk '($0 !~ /^#/ || $0 ~ /^#HttpOnly_/) && NF >= 7 {print $7}' "$cookie_jar")
}

website_login() {
    local cookie_jar=$1
    local username=$2
    local password=$3
    local page
    page=$(mktemp /var/tmp/geoflow-phase-c-login.XXXXXX)
    website_get "$cookie_jar" login "$page"
    test "$website_status" = 200
    local csrf_token
    csrf_token=$(extract_csrf_token "$page")
    mask_value "$csrf_token"
    website_post "$cookie_jar" login "$csrf_token" "$page" \
        "username=$username" "password=$password" 'remember=0'
    test "$website_status" = 302
    website_get "$cookie_jar" dashboard "$page"
    test "$website_status" = 200
    mask_cookie_jar "$cookie_jar"
    rm -f "$page"
}

current_operation_id() {
    api_request GET operations/current "" ""
    if [[ $api_status == 200 ]]; then
        jq -er '.id' <<< "$api_body"
    elif [[ $api_status == 404 ]]; then
        printf '\n'
    else
        echo "Unable to read the current updater operation." >&2
        return 1
    fi
}

website_expect_validation_error() {
    local expected_marker=$1
    local expected_error_marker=$2
    local path=$3
    shift 3
    local page
    page=$(mktemp /var/tmp/geoflow-phase-c-validation.XXXXXX)
    website_get "$website_cookie_jar" system-updates "$page"
    test "$website_status" = 200
    local csrf_token
    csrf_token=$(extract_csrf_token "$page")
    mask_value "$csrf_token"
    local previous_operation
    previous_operation=$(current_operation_id)
    website_post "$website_cookie_jar" "$path" "$csrf_token" "$page" "$@"
    test "$website_status" = 302
    scan_runtime_logs
    website_get "$website_cookie_jar" system-updates "$page"
    test "$website_status" = 200
    grep -Fq "$expected_error_marker" "$page"
    if [[ -n $expected_marker ]]; then
        grep -Fq "$expected_marker" "$page"
    fi
    test "$(current_operation_id)" = "$previous_operation"
    rm -f "$page"
}

website_start_operation() {
    local path=$1
    shift
    local page
    page=$(mktemp /var/tmp/geoflow-phase-c-operation.XXXXXX)
    website_get "$website_cookie_jar" system-updates "$page"
    test "$website_status" = 200
    local csrf_token
    csrf_token=$(extract_csrf_token "$page")
    mask_value "$csrf_token"
    local previous_operation
    previous_operation=$(current_operation_id)
    website_post "$website_cookie_jar" "$path" "$csrf_token" "$page" "$@"
    test "$website_status" = 302
    test -n "$website_location"
    scan_runtime_logs
    operation_id=""
    for _ in $(seq 1 30); do
        api_request GET operations/current "" ""
        if [[ $api_status == 200 ]]; then
            operation_id=$(jq -er '.id' <<< "$api_body")
            if [[ $operation_id != "$previous_operation" ]]; then
                rm -f "$page"
                return 0
            fi
        fi
        sleep 1
    done
    rm -f "$page"
    echo "The administrator update center did not start a new operation." >&2
    return 1
}

restore_fixture_id=""
restore_fixture_value=""
restore_site_env_sha=""
restore_instance_sha=""
restore_release_env_sha=""
restore_compose_sha=""
restore_migrations_sha=""

prepare_restore_fixture() {
    local scenario=$1
    if [[ ! $scenario =~ ^[a-z0-9-]+$ ]]; then
        echo "Restore scenario name is invalid." >&2
        return 1
    fi
    restore_fixture_id=phase-c-${scenario}-${ARCHIVE_ARCH}-${candidate_run_id}
    restore_fixture_value=clean-$scenario
    docker exec geoflow-postgres-prod psql --username=geo_user --dbname=geo_flow --set ON_ERROR_STOP=1 \
        --command "INSERT INTO system_update_runs (run_uuid, action, status, current_version, target_version, created_at, updated_at) VALUES ('${restore_fixture_id}', 'apply', 'completed', '2.3.0', '${restore_fixture_value}', NOW(), NOW()) ON CONFLICT (run_uuid) DO UPDATE SET target_version=EXCLUDED.target_version, updated_at=NOW()" >/dev/null
    docker exec --env REDISCLI_AUTH="$redis_password" geoflow-redis-prod redis-cli \
        SET "geoflow:${restore_fixture_id}:fault-restore" "$restore_fixture_value" >/dev/null 2>&1
    printf '%s\n' "$restore_fixture_value" > "$instance_root/storage/app/phase-c-fault-restore.txt"
    if grep -q '^PHASE_C_FAULT_RESTORE_MARKER=' "$instance_root/.env.prod"; then
        sed -i "s/^PHASE_C_FAULT_RESTORE_MARKER=.*/PHASE_C_FAULT_RESTORE_MARKER=${restore_fixture_value}/" "$instance_root/.env.prod"
    else
        printf 'PHASE_C_FAULT_RESTORE_MARKER=%s\n' "$restore_fixture_value" >> "$instance_root/.env.prod"
    fi
    restore_site_env_sha=$(sha256sum "$instance_root/.env.prod" | cut -d ' ' -f 1)
    restore_instance_sha=$(sha256sum "$instance_dir/instance.yml" | cut -d ' ' -f 1)
    restore_release_env_sha=$(sha256sum "$instance_dir/release.env" | cut -d ' ' -f 1)
    restore_compose_sha=$(sha256sum "$instance_dir/docker-compose.managed.yml" | cut -d ' ' -f 1)
    restore_migrations_sha=$(docker exec geoflow-postgres-prod psql --username=geo_user --dbname=geo_flow --tuples-only --no-align \
        --command "SELECT migration || ':' || batch FROM migrations ORDER BY id" | sha256sum | cut -d ' ' -f 1)
    printf '%s\n' "$restore_fixture_id" > "$state_dir/rehearsal-restore-fixture"
    rm -f "$state_dir/rehearsal-corruption-applied"
}

assert_restore_fixture() {
    test "$(<"$state_dir/rehearsal-corruption-applied")" = "$restore_fixture_id"
    test "$(docker exec geoflow-postgres-prod psql --username=geo_user --dbname=geo_flow --tuples-only --no-align --command "SELECT target_version FROM system_update_runs WHERE run_uuid='${restore_fixture_id}'")" = "$restore_fixture_value"
    test "$(docker exec --env REDISCLI_AUTH="$redis_password" geoflow-redis-prod redis-cli GET "geoflow:${restore_fixture_id}:fault-restore" 2>/dev/null)" = "$restore_fixture_value"
    test "$(<"$instance_root/storage/app/phase-c-fault-restore.txt")" = "$restore_fixture_value"
    grep -qx "PHASE_C_FAULT_RESTORE_MARKER=${restore_fixture_value}" "$instance_root/.env.prod"
    test "$(sha256sum "$instance_root/.env.prod" | cut -d ' ' -f 1)" = "$restore_site_env_sha"
    test "$(sha256sum "$instance_dir/instance.yml" | cut -d ' ' -f 1)" = "$restore_instance_sha"
    test "$(sha256sum "$instance_dir/release.env" | cut -d ' ' -f 1)" = "$restore_release_env_sha"
    test "$(sha256sum "$instance_dir/docker-compose.managed.yml" | cut -d ' ' -f 1)" = "$restore_compose_sha"
    test "$(docker exec geoflow-postgres-prod psql --username=geo_user --dbname=geo_flow --tuples-only --no-align \
        --command "SELECT migration || ':' || batch FROM migrations ORDER BY id" | sha256sum | cut -d ' ' -f 1)" = "$restore_migrations_sha"
    test -z "$(docker ps -a --filter 'name=^/geoflow-system-update-queue-prod$' --format '{{.Names}}')"
    geoflow-updater doctor --instance primary --json > "$evidence_dir/doctor-${restore_fixture_id}.json"
    test "$(jq -er '.status' "$evidence_dir/doctor-${restore_fixture_id}.json")" = pass
    verify_fpm_bridge_permissions "$evidence_dir/fpm-${restore_fixture_id}.json"
    local operation_state_sha
    operation_state_sha=$(sha256sum "$instance_dir/operations/current.json" | cut -d ' ' -f 1)
    systemctl restart geoflow-updater.service
    wait_for_service
    verify_restarted_website_bridge "restore-${restore_fixture_id}" phase-b
    test "$(current_operation_id)" = "$operation_id"
    test "$(sha256sum "$instance_dir/operations/current.json" | cut -d ' ' -f 1)" = "$operation_state_sha"
}

verify_fpm_bridge_permissions() {
    docker exec -i geoflow-app-prod php <<'PHP' | jq . > "$1"
<?php
$token = lstat('/run/secrets/geoflow-updater-control-token');
$directory = lstat('/run/geoflow-updater');
$socket = lstat('/run/geoflow-updater/geoflow-updater.sock');
$gid = $token['gid'];
foreach ([[$token, 0100640], [$directory, 0040750], [$socket, 0140660]] as [$stat, $mode]) {
    if ($gid <= 0 || $stat['uid'] !== 0 || $stat['gid'] !== $gid || ($stat['mode'] & 0177777) !== $mode) {
        throw new RuntimeException('Updater ownership or permissions changed.');
    }
}
$workers = 0;
foreach (glob('/proc/[0-9]*/status') as $path) {
    $status = @file_get_contents($path);
    if (!is_string($status) || !preg_match('/^Name:\s+php-fpm\s*$/m', $status)
        || !preg_match('/^Uid:\s+\d+\s+(\d+)/m', $status, $uid) || (int) $uid[1] === 0) {
        continue;
    }
    preg_match('/^Gid:\s+\d+\s+(\d+)/m', $status, $group);
    preg_match('/^Groups:([^\n]*)/m', $status, $groups);
    if ((int) $uid[1] !== 33 || (int) $group[1] !== $gid
        || !in_array((string) $gid, preg_split('/\s+/', trim($groups[1])), true)) {
        throw new RuntimeException('Real FPM worker lost its non-root bridge identity.');
    }
    $workers++;
}
if ($workers === 0) {
    throw new RuntimeException('No real FPM workers were inspected.');
}
echo json_encode(['worker_count' => $workers, 'worker_uid' => 33, 'bridge_gid' => $gid,
    'token_mode' => '0640', 'directory_mode' => '0750', 'socket_mode' => '0660'], JSON_THROW_ON_ERROR);
PHP
}

run_bridge_probe() {
    local mode=$1
    local output=$2
    verify_fpm_bridge_permissions "${output%.json}-fpm-permissions.json"
    local php_source
    if [[ $mode == phase-b ]]; then
        php_source='require "/var/www/html/vendor/autoload.php"; $app = require "/var/www/html/bootstrap/app.php"; $app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap(); $client = $app->make(App\Contracts\SystemUpdater\AgentClient::class); $status = $client->status(); $policy = $app->make(App\Services\Admin\SystemUpdaterMutationPolicy::class); echo json_encode(["status" => $status["status"], "update" => $policy->allows($status, "update"), "backup" => $policy->allows($status, "backup"), "rollback" => $policy->allows($status, "rollback")], JSON_THROW_ON_ERROR);'
    else
        php_source='require "/var/www/html/vendor/autoload.php"; $app = require "/var/www/html/bootstrap/app.php"; $app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap(); $client = $app->make(App\Contracts\SystemUpdater\AgentClient::class); $status = $client->status(); $points = $client->recoveryPoints(); $history = $app->make(App\Services\Admin\SystemUpdateStateService::class); $recent = $history->summary("recent"); $archived = $history->summary("archived"); $routes = $app->make("router")->getRoutes(); $marker = getenv("PHASE_C_MARKER"); echo json_encode(["status" => $status["status"], "recovery_points" => count($points), "recent_visible" => $recent["recent_runs"]->getCollection()->contains("run_uuid", $marker."-recent"), "archive_visible" => $archived["recent_runs"]->getCollection()->contains("run_uuid", $marker."-archive"), "run_detail_methods" => $routes->getByName("admin.system-updates.runs.show")->methods(), "backup_detail_methods" => $routes->getByName("admin.system-updates.backups.show")->methods()], JSON_THROW_ON_ERROR);'
    fi
    docker exec --env "PHASE_C_MARKER=${marker:-}" geoflow-app-prod php -r "$php_source" | jq . > "$output"
}

verify_restarted_website_bridge() {
    local label=$1
    local phase=$2
    local page
    page=$(mktemp /var/tmp/geoflow-phase-c-restart-page.XXXXXX)
    website_get "$website_cookie_jar" system-updates "$page"
    local marker='name="updater_authorization_code"'
    if [[ $phase == phase-c ]]; then
        marker=data-system-updater-authorized-action
    fi
    local controls_available=false
    if grep -Fq "$marker" "$page"; then
        controls_available=true
    fi
    jq -n --arg http_status "$website_status" --argjson controls_available "$controls_available" \
        '{http_status:$http_status,controls_available:$controls_available}' \
        > "$evidence_dir/website-restart-${label}.json"
    rm -f "$page"
    test "$website_status" = 200
    test "$controls_available" = true
    verify_fpm_bridge_permissions "$evidence_dir/fpm-restart-${label}.json"
}

current_check=native-host
for command_name in curl docker find git go jq openssl python3 sha256sum systemctl; do
    require_command "$command_name"
done
test "$(uname -s)" = Linux
case "${PLATFORM:-}" in
    linux-amd64)
        test "$(uname -m)" = x86_64
        test "${ARCHIVE_ARCH:-}" = amd64
        ;;
    linux-arm64)
        test "$(uname -m)" = aarch64
        test "${ARCHIVE_ARCH:-}" = arm64
        ;;
    *)
        echo "Unsupported native platform." >&2
        exit 1
        ;;
esac
record_check native-host "GitHub-hosted $(uname -m) Linux VM; no architecture emulation"

current_check=native-updater-tests
log "Running updater tests on the native host"
: > "$evidence_dir/go-test.txt"
modules_ready=false
for attempt in 1 2 3; do
    if (cd "$updater_root" && go mod download) >> "$evidence_dir/go-test.txt" 2>&1; then
        modules_ready=true
        break
    fi
    if (( attempt < 3 )); then
        printf 'Go module download failed; retrying attempt %d of 3.\n' "$((attempt + 1))" | tee -a "$evidence_dir/go-test.txt"
        sleep "$((attempt * 5))"
    fi
done
test "$modules_ready" = true
(cd "$updater_root" && go test -race ./...) 2>&1 | tee -a "$evidence_dir/go-test.txt"
record_check native-updater-tests "Full Go race suite, including protected-stage rollback, restart reconciliation, persistent recovery backoff, and authorization expiry"

current_check=candidate-install
candidate_run_id=$(jq -er '.candidate_run_id | tostring' "$candidate_dir/candidate.json")
test "$candidate_run_id" = "${CANDIDATE_RUN_ID:-}"
updater_version=$(jq -er '.updater.version' "$candidate_dir/candidate.json")
archive=$candidate_dir/dist/geoflow-updater_${updater_version}_linux_${ARCHIVE_ARCH}.tar.gz
test -f "$archive"
expected_archive_sha=$(jq -er --arg platform "$PLATFORM" '.updater.archives[$platform]' "$candidate_dir/candidate.json")
test "$(sha256sum "$archive" | cut -d ' ' -f 1)" = "$expected_archive_sha"
archive_root=$(mktemp -d /var/tmp/geoflow-updater-candidate.XXXXXX)
tar -xzf "$archive" -C "$archive_root"
bash "$archive_root/packaging/scripts/install.sh"
test "$(geoflow-updater version)" = "$updater_version"
record_check candidate-install "Candidate archive checksum, native execution, installer, systemd unit"

current_check=phase-b-deployment
test ! -e "$instance_root"
install -d -o root -g root -m 0755 "$instance_root"
find "$phase_b_root" -mindepth 1 -maxdepth 1 ! -name .git -exec cp -a -- {} "$instance_root/" \;
cp "$instance_root/.env.prod.example" "$instance_root/.env.prod"
app_key="base64:$(openssl rand -base64 32)"
db_password=$(openssl rand -hex 24)
redis_password=$(openssl rand -hex 24)
admin_password=$(openssl rand -base64 36 | tr -d '\n')
reverb_secret=$(openssl rand -hex 24)
for secret_value in "$app_key" "$db_password" "$redis_password" "$admin_password" "$reverb_secret"; do
    mask_value "$secret_value"
done
PHASE_C_APP_KEY=$app_key \
PHASE_C_DB_PASSWORD=$db_password \
PHASE_C_REDIS_PASSWORD=$redis_password \
PHASE_C_ADMIN_PASSWORD=$admin_password \
PHASE_C_REVERB_SECRET=$reverb_secret \
PHASE_C_INSTANCE_ROOT=$instance_root \
python3 - "$instance_root/.env.prod" <<'PY'
import os
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
replacements = {
    "APP_KEY": os.environ["PHASE_C_APP_KEY"],
    "APP_URL": "http://localhost:18080",
    "SESSION_SECURE_COOKIE": "false",
    "DB_PASSWORD": os.environ["PHASE_C_DB_PASSWORD"],
    "REDIS_PASSWORD": os.environ["PHASE_C_REDIS_PASSWORD"],
    "GEOFLOW_ADMIN_PASSWORD": os.environ["PHASE_C_ADMIN_PASSWORD"],
    "REVERB_APP_SECRET": os.environ["PHASE_C_REVERB_SECRET"],
    "POSTGRES_DATA_DIR": os.environ["PHASE_C_INSTANCE_ROOT"] + "/docker-data/prod/postgres",
    "POSTGRES_CONTAINER_DATA_DIR": "/var/lib/postgresql",
    "PGVECTOR_IMAGE": "pgvector/pgvector:pg18",
    "REDIS_IMAGE": "redis:8-alpine",
    "WEB_PORT": "18080",
    "REVERB_EXPOSE_PORT": "18081",
    "GEOFLOW_TELEMETRY_ENABLED": "false",
    "GEOFLOW_UPDATE_CHECK_ENABLED": "false",
    "GEOFLOW_UPDATE_REQUIRE_ADMIN_PASSWORD": "true",
    "GEOFLOW_INITIAL_ADMIN_HINT_ENABLED": "false",
}
seen = set()
output = []
for line in path.read_text().splitlines():
    key = line.split("=", 1)[0].strip()
    if key in replacements:
        output.append(f"{key}={replacements[key]}")
        seen.add(key)
    else:
        output.append(line)
missing = sorted(set(replacements) - seen)
if missing:
    raise SystemExit("missing environment keys: " + ", ".join(missing))
path.write_text("\n".join(output) + "\n")
PY
printf '%s\n' \
    'DOCKER_NETWORK_NAME=geoflow-phase-c-rehearsal-net' \
    'GEOFLOW_PRIMARY_HOSTS=localhost' \
    'GEOFLOW_NGINX_PRIMARY_HOST=localhost' \
    'GEOFLOW_NGINX_PRIMARY_ALIASES=' \
    'GEOFLOW_NGINX_PUBLIC_SCHEME=http' \
    'GEOFLOW_NGINX_PUBLIC_PORT=18080' \
    'PHASE_C_REHEARSAL_MARKER=before-update' \
    >> "$instance_root/.env.prod"
mkdir -p "$instance_root/storage/app" "$instance_root/docker-data/prod/postgres" "$instance_root/docker-data/prod/redis"
printf 'before-update\n' > "$instance_root/storage/app/phase-c-rehearsal.txt"
compose_source=(docker compose --env-file "$instance_root/.env.prod" -f "$instance_root/docker-compose.prod.yml")
log "Building and starting the isolated Phase B source deployment"
"${compose_source[@]}" up -d --build --wait --wait-timeout 600
curl --fail --silent --show-error --header 'Host: localhost' http://127.0.0.1:18080/up >/dev/null
test "$(jq -er '.version' "$instance_root/version.json")" = 2.3.0
record_check phase-b-deployment "Fresh Phase B source deployment under /opt on the disposable runner"

current_check=pre-cutover-idle
marker="phase-c-${PLATFORM}-${candidate_run_id}"
docker exec geoflow-postgres-prod psql --username=geo_user --dbname=geo_flow --set ON_ERROR_STOP=1 \
    --command "INSERT INTO system_update_runs (run_uuid, action, status, current_version, target_version, created_at, updated_at) VALUES ('${marker}-recent', 'apply', 'completed', '2.3.0', '2.3.0', NOW(), NOW()), ('${marker}-archive', 'rollback', 'completed', '2.3.0', '2.3.0', NOW() - INTERVAL '100 days', NOW() - INTERVAL '100 days') ON CONFLICT (run_uuid) DO NOTHING" >/dev/null
pending_rows=$(docker exec geoflow-postgres-prod psql --username=geo_user --dbname=geo_flow --tuples-only --no-align \
    --command "SELECT count(*) FROM system_update_runs WHERE action IN ('apply','rollback') AND status IN ('queued','running')")
test "$pending_rows" = 0
pending_jobs=$(docker exec --env REDISCLI_AUTH="$redis_password" geoflow-redis-prod redis-cli LLEN queues:system-updates 2>/dev/null)
reserved_jobs=$(docker exec --env REDISCLI_AUTH="$redis_password" geoflow-redis-prod redis-cli ZCARD queues:system-updates:reserved 2>/dev/null)
delayed_jobs=$(docker exec --env REDISCLI_AUTH="$redis_password" geoflow-redis-prod redis-cli ZCARD queues:system-updates:delayed 2>/dev/null)
test "$((pending_jobs + reserved_jobs + delayed_jobs))" = 0
docker exec --env REDISCLI_AUTH="$redis_password" geoflow-redis-prod redis-cli SET "geoflow:${marker}:before" before-update >/dev/null 2>&1
record_check pre-cutover-idle "No queued/running legacy mutation row and no pending/reserved system-updates job"

current_check=enrollment-boundary
test ! -e "$unsafe_root"
mkdir -p "$unsafe_root/storage"
cp "$instance_root/.env.prod" "$unsafe_root/.env.prod"
cp "$instance_root/version.json" "$unsafe_root/version.json"
if geoflow-updater enroll --instance-root "$unsafe_root" > "$evidence_dir/unsafe-enrollment.txt" 2>&1; then
    echo "Enrollment accepted a systemd-blocked root." >&2
    exit 1
fi
geoflow-updater enroll --instance-root "$instance_root" > "$evidence_dir/enrollment.txt"
test -f "$instance_dir/instance.yml"
control_token=$(tr -d '\n' < "$instance_dir/control.token")
mask_value "$control_token"
record_check enrollment-boundary "Blocked /tmp root and enrolled canonical /opt root against the signed Phase B release"

current_check=phase-b-compose-compatibility
phase_b_compose_mode=$(stat -c '%a:%u:%g' "$instance_dir/docker-compose.managed.yml")
test "$(sha256sum "$instance_dir/docker-compose.managed.yml" | cut -d ' ' -f 1)" = a6a8b6ca1f0e7c9c00c4093a237206a6c24131fc94e5540c3b1dfd1fe84dcc67
PHASE_C_COMPOSE_PATH=$instance_dir/docker-compose.managed.yml python3 - <<'PY'
import os
import pathlib

path = pathlib.Path(os.environ["PHASE_C_COMPOSE_PATH"])
contents = path.read_text()
old = """      test: ["CMD-SHELL", "wget -q --header='Host: $${GEOFLOW_NGINX_PRIMARY_HOST}' -O /dev/null http://127.0.0.1/up"]"""
new = r"""      test: ["CMD-SHELL", "wget -q --header=\"Host: $${GEOFLOW_NGINX_PRIMARY_HOST}\" -O /dev/null http://127.0.0.1/up"]"""
app = "    container_name: geoflow-app-prod\n"
worker_group = r"""    command:
      - /bin/sh
      - -ec
      - |
        bridge_gid=$$(stat -c '%g' /run/secrets/geoflow-updater-control-token)
        case "$$bridge_gid" in ''|*[!0-9]*) exit 1 ;; esac
        test "$$bridge_gid" -gt 0
        printf '[www]\ngroup = %s\n' "$$bridge_gid" > /usr/local/etc/php-fpm.d/zzz-geoflow-updater-group.conf
        exec php-fpm -F
"""
if contents.count(old) != 1 or contents.count(app) != 1:
    raise SystemExit("the signed Phase B Compose compatibility defect did not match exactly once")
path.write_text(contents.replace(old, new).replace(app, app + worker_group))
PY
test "$(stat -c '%a:%u:%g' "$instance_dir/docker-compose.managed.yml")" = "$phase_b_compose_mode"
test "$(sha256sum "$instance_dir/docker-compose.managed.yml" | cut -d ' ' -f 1)" = f5b7d6d0ee8fcfda7df7777126785d99ac81e648bd8c4dd4b9f2c2c41f1ca498
cmp "$instance_dir/docker-compose.managed.yml" "$updater_root/assets/docker-compose.managed.yml"
record_check phase-b-compose-compatibility "Verified signed sequence-1 Compose and applied exact Host healthcheck and FPM worker-group corrections without changing credential permissions"

current_check=managed-phase-b-handover
managed_compose=(docker compose --env-file "$instance_root/.env.prod" --env-file "$instance_dir/release.env" -f "$instance_dir/docker-compose.managed.yml")
"${managed_compose[@]}" down --remove-orphans
"${managed_compose[@]}" up -d --remove-orphans --wait --wait-timeout 600
stable_app_image=$(awk -F= '$1 == "GEOFLOW_APP_IMAGE" {print substr($0, index($0, "=")+1)}' "$instance_dir/release.env")
test -n "$stable_app_image"
network_name=$(read_environment_value DOCKER_NETWORK_NAME)
test "$network_name" = geoflow-phase-c-rehearsal-net
docker run -d \
    --name geoflow-system-update-queue-prod \
    --restart unless-stopped \
    --network "$network_name" \
    --user www-data:www-data \
    --env-file "$instance_root/.env.prod" \
    --env AUTO_FIX_STORAGE_PERMISSIONS=false \
    --env AUTO_INSTALL_ONCE=false \
    --env AUTO_MIGRATE=false \
    --volume "$instance_root/.env.prod:/var/www/html/.env:ro" \
    --volume "$instance_root/storage:/var/www/html/storage" \
    "$stable_app_image" \
    php -d memory_limit=256M artisan queue:work redis --queue=system-updates --sleep=1 --tries=1 --timeout=930 >/dev/null
test "$(docker inspect --format '{{.State.Running}}' geoflow-system-update-queue-prod)" = true
record_check managed-phase-b-handover "Signed Phase B managed deployment plus a real lingering retired worker for the bounded handover"

current_check=authorization-factors
authorization_output=$(geoflow-updater authorization-uri --instance primary)
for scope in update backup rollback; do
    uri=$(sed -n "s/^${scope}: //p" <<< "$authorization_output")
    test -n "$uri"
    mask_value "$uri"
    secret=$(TOTP_URI="$uri" python3 - <<'PY'
import os
import urllib.parse

uri = urllib.parse.urlparse(os.environ["TOTP_URI"])
values = urllib.parse.parse_qs(uri.query, strict_parsing=True)
print(values["secret"][0])
PY
)
    test -n "$secret"
    factor_secrets[$scope]=$secret
    mask_value "$secret"
done
test "${factor_secrets[update]}" != "${factor_secrets[backup]}"
test "${factor_secrets[update]}" != "${factor_secrets[rollback]}"
test "${factor_secrets[backup]}" != "${factor_secrets[rollback]}"
test "$(stat -c '%a' "$instance_dir/mutation.secret")" = 600
record_check authorization-factors "Three distinct scoped factors; master secret is root-only"

current_check=phase-b-doctor
if geoflow-updater doctor --instance primary --json > "$evidence_dir/doctor-phase-b.json"; then
    echo "Phase B diagnostics unexpectedly passed with the retired worker running." >&2
    exit 1
fi
test "$(jq -r '.status' "$evidence_dir/doctor-phase-b.json")" = fail
test "$(jq -c '[.checks[] | select(.status != "pass") | .id]' "$evidence_dir/doctor-phase-b.json")" = '["retired-update-worker"]'
run_bridge_probe phase-b "$evidence_dir/website-phase-b-policy.json"
test "$(jq -r '.update' "$evidence_dir/website-phase-b-policy.json")" = true
test "$(jq -r '.backup' "$evidence_dir/website-phase-b-policy.json")" = false
test "$(jq -r '.rollback' "$evidence_dir/website-phase-b-policy.json")" = false
record_check phase-b-doctor "Exactly retired-update-worker fails; website policy permits only the signed update handover"

current_check=website-security-boundary
regular_admin_password=$(openssl rand -base64 36 | tr -d '\n')
wrong_admin_password=$(openssl rand -base64 36 | tr -d '\n')
mask_value "$regular_admin_password"
mask_value "$wrong_admin_password"
docker exec --env PHASE_C_REGULAR_ADMIN_PASSWORD="$regular_admin_password" geoflow-app-prod php -r \
    'require "/var/www/html/vendor/autoload.php"; $app = require "/var/www/html/bootstrap/app.php"; $app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap(); App\Models\Admin::query()->updateOrCreate(["username" => "phase_c_operator"], ["email" => "phase-c-operator@example.invalid", "display_name" => "Phase C Operator", "password" => getenv("PHASE_C_REGULAR_ADMIN_PASSWORD"), "role" => "admin", "status" => "active"]);'
website_cookie_jar=$(mktemp /var/tmp/geoflow-phase-c-super-cookie.XXXXXX)
regular_cookie_jar=$(mktemp /var/tmp/geoflow-phase-c-regular-cookie.XXXXXX)
anonymous_cookie_jar=$(mktemp /var/tmp/geoflow-phase-c-anonymous-cookie.XXXXXX)
website_login "$website_cookie_jar" admin "$admin_password"
website_login "$regular_cookie_jar" phase_c_operator "$regular_admin_password"
boundary_page=$(mktemp /var/tmp/geoflow-phase-c-boundary.XXXXXX)
website_get "$regular_cookie_jar" system-updates "$boundary_page"
test "$website_status" = 403
website_get "$anonymous_cookie_jar" login "$boundary_page"
test "$website_status" = 200
anonymous_csrf=$(extract_csrf_token "$boundary_page")
mask_value "$anonymous_csrf"
previous_operation=$(current_operation_id)
website_post "$anonymous_cookie_jar" system-updates/updater/update "$anonymous_csrf" "$boundary_page" \
    "current_admin_password=$admin_password" 'updater_authorization_code=000000'
test "$website_status" = 302
[[ $website_location == *'/geo_admin/login' ]]
test "$(current_operation_id)" = "$previous_operation"
website_post "$website_cookie_jar" system-updates/updater/update "" "$boundary_page" \
    "current_admin_password=$admin_password" 'updater_authorization_code=000000'
test "$website_status" = 419
test "$(current_operation_id)" = "$previous_operation"
website_expect_validation_error "" admin-flash-alert system-updates/updater/update \
    "current_admin_password=$admin_password"
website_expect_validation_error '管理员密码不正确。' admin-flash-alert system-updates/updater/update \
    "current_admin_password=$wrong_admin_password" 'updater_authorization_code=000000'
website_get "$website_cookie_jar" system-updates "$boundary_page"
test "$website_status" = 200
grep -Fq 'name="updater_authorization_code"' "$boundary_page"
grep -Fq 'name="current_admin_password"' "$boundary_page"
scan_runtime_logs
rm -f "$boundary_page"
record_check website-security-boundary "Real HTTP sessions enforced login, super-admin authorization, CSRF, six-digit authorization input, and current administrator password"

current_check=candidate-repository
certificate_dir=$(mktemp -d /var/tmp/geoflow-candidate-tls.XXXXXX)
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -sha256 \
    -subj '/CN=GEOFlow Phase C Rehearsal CA' \
    -keyout "$certificate_dir/ca.key" -out "$certificate_dir/ca.crt" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -sha256 \
    -subj '/CN=127.0.0.1' \
    -keyout "$certificate_dir/server.key" -out "$certificate_dir/server.csr" >/dev/null 2>&1
printf 'subjectAltName=IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' > "$certificate_dir/server.ext"
openssl x509 -req -days 2 -sha256 \
    -in "$certificate_dir/server.csr" \
    -CA "$certificate_dir/ca.crt" -CAkey "$certificate_dir/ca.key" -CAcreateserial \
    -extfile "$certificate_dir/server.ext" \
    -out "$certificate_dir/server.crt" >/dev/null 2>&1
install -o root -g root -m 0644 "$certificate_dir/ca.crt" /usr/local/share/ca-certificates/geoflow-phase-c-rehearsal.crt
update-ca-certificates >/dev/null
python3 "$updater_root/scripts/serve-rehearsal-repository.py" \
    --directory "$candidate_dir/tuf/repository" \
    --certificate "$certificate_dir/server.crt" \
    --private-key "$certificate_dir/server.key" \
    --port "$repository_port" > "$evidence_dir/repository-server.txt" 2>&1 &
repository_pid=$!
for _ in $(seq 1 30); do
    if curl --fail --silent --show-error "https://127.0.0.1:${repository_port}/metadata/timestamp.json" >/dev/null; then
        break
    fi
    sleep 1
done
curl --fail --silent --show-error "https://127.0.0.1:${repository_port}/metadata/timestamp.json" >/dev/null
install -d -o root -g root -m 0755 /etc/systemd/system/geoflow-updater.service.d
install -d -o root -g root -m 0755 /usr/local/lib/geoflow-phase-c-rehearsal/bin
install -o root -g root -m 0755 "$updater_root/scripts/rehearsal-docker-wrapper.sh" /usr/local/lib/geoflow-phase-c-rehearsal/bin/docker
printf '%s\n' \
    '[Service]' \
    'Environment=GEOFLOW_UPDATER_ALLOW_CANDIDATE_REPOSITORY=1' \
    "Environment=GEOFLOW_UPDATER_TUF_METADATA_URL=https://127.0.0.1:${repository_port}/metadata" \
    "Environment=GEOFLOW_UPDATER_TUF_TARGETS_URL=https://127.0.0.1:${repository_port}/targets" \
    'Environment=PATH=/usr/local/lib/geoflow-phase-c-rehearsal/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin' \
    > /etc/systemd/system/geoflow-updater.service.d/phase-c-rehearsal.conf
chmod 0600 /etc/systemd/system/geoflow-updater.service.d/phase-c-rehearsal.conf
systemctl daemon-reload
systemctl restart geoflow-updater.service
systemctl is-active --quiet geoflow-updater.service
test "$(stat -c '%a' /etc/systemd/system/geoflow-updater.service.d/phase-c-rehearsal.conf)" = 600
record_check candidate-repository "Root-only candidate opt-in and private HTTPS TUF origin"

current_check=website-bridge-after-restart
wait_for_service
verify_restarted_website_bridge candidate-repository phase-b
record_check website-bridge-after-restart "Authenticated website controls and non-root FPM bridge permissions survive updater restart"
current_check=diagnostic-restart-regression-no-publication
exit 0

current_check=mutation-authorization
expect_api_error POST updates "" "" 403 mutation_authorization_required
expect_api_error POST backups "" "" 403 mutation_authorization_required
api_request GET backups "" ""
test "$api_status" = 200
test "$(jq -r '.schema_version' <<< "$api_body")" = 1
next_totp update
update_code=$totp_code
expect_api_error POST backups "$update_code" "" 403 mutation_authorization_invalid
website_start_operation system-updates/updater/update \
    "current_admin_password=$admin_password" "updater_authorization_code=$update_code"
first_update_operation=$operation_id
expect_api_error POST updates "$update_code" "" 403 mutation_authorization_replayed
record_check mutation-authorization "Missing, cross-scope, and replayed codes rejected; the authenticated administrator HTTP route relayed one accepted update"

current_check=signed-update
wait_for_operation first-update succeeded update
test "$operation_id" = "$first_update_operation"
test "$(jq -er '.target_version' "$evidence_dir/operation-first-update.json")" = 3.0.0
for stage in resolve preflight pull quiesce backup migrate activate resume verify succeeded; do
    test "$(jq -r --arg stage "$stage" 'any(.stages[]; .name == $stage and .status == "succeeded")' "$evidence_dir/operation-first-update.json")" = true
done
update_checkpoint=$operation_recovery_point
test -n "$update_checkpoint"
test "$(jq -er '.version' "$instance_root/version.json")" = 3.0.0
record_check signed-update "Signed 2.3.0 to 3.0.0 update completed every durable stage"

current_check=backup-completeness
checkpoint_manifest=$backup_root/$update_checkpoint/manifest.json
test -f "$checkpoint_manifest"
for backup_file in database.dump storage.tar.gz redis.tar.gz site.env version.json managed/instance.yml managed/release.env managed/docker-compose.yml; do
    test -f "$backup_root/$update_checkpoint/$backup_file"
    test "$(jq -r --arg file "$backup_file" '.files[$file].sha256 // empty' "$checkpoint_manifest" | wc -c)" -gt 1
done
test "$(jq -er '.version' "$checkpoint_manifest")" = 2.3.0
test "$(jq -er '.release_sequence' "$checkpoint_manifest")" = 1
record_check backup-completeness "PostgreSQL, Redis, storage, site environment, version marker, and managed deployment state"

current_check=managed-activation
test -z "$(docker ps -a --filter 'name=^/geoflow-system-update-queue-prod$' --format '{{.Names}}')"
if grep -Eq '^[[:space:]]+system-update-queue:' "$instance_dir/docker-compose.managed.yml"; then
    echo "Activated Compose retains the retired service." >&2
    exit 1
fi
geoflow-updater doctor --instance primary --json > "$evidence_dir/doctor-v3.json"
test "$(jq -r '.status' "$evidence_dir/doctor-v3.json")" = pass
test "$(jq -r '[.checks[] | select(.status != "pass")] | length' "$evidence_dir/doctor-v3.json")" = 0
docker exec geoflow-app-prod test ! -e /var/lib/geoflow-updater/instances/primary/mutation.secret
record_check managed-activation "Retired container removed, no retired service in Compose, all diagnostics pass, master factor absent from application"

current_check=website-bridge
run_bridge_probe phase-c "$evidence_dir/website-phase-c-bridge.json"
test "$(jq -r '.status' "$evidence_dir/website-phase-c-bridge.json")" = pass
test "$(jq -r '.recent_visible' "$evidence_dir/website-phase-c-bridge.json")" = true
test "$(jq -r '.archive_visible' "$evidence_dir/website-phase-c-bridge.json")" = true
test "$(jq -c '.run_detail_methods' "$evidence_dir/website-phase-c-bridge.json")" = '["GET","HEAD"]'
test "$(jq -c '.backup_detail_methods' "$evidence_dir/website-phase-c-bridge.json")" = '["GET","HEAD"]'
phase_c_update_page=$(mktemp /var/tmp/geoflow-phase-c-update-center.XXXXXX)
website_get "$website_cookie_jar" system-updates "$phase_c_update_page"
test "$website_status" = 200
grep -Fq 'data-system-updater-authorized-action' "$phase_c_update_page"
rm -f "$phase_c_update_page"
website_start_operation system-updates/updater/verify
wait_for_operation website-verify succeeded verify
geoflow-updater recovery-points --instance primary > "$evidence_dir/recovery-points-after-update.json"
test "$(jq -r --arg id "$update_checkpoint" 'any(.[]; .id == $id and (.reason | startswith("update-to-")))' "$evidence_dir/recovery-points-after-update.json")" = true
record_check website-bridge "Authenticated super-admin HTTP exercised verification; application bridge exposed status, recovery points, 90-day history/archive split, and GET-only legacy details"

current_check=manual-backup
expect_api_error POST backups "" "" 403 mutation_authorization_required
next_totp backup
website_start_operation system-updates/updater/backup \
    "current_admin_password=$admin_password" "updater_authorization_code=$totp_code"
wait_for_operation manual-backup succeeded backup
manual_backup=$operation_recovery_point
test -n "$manual_backup"
test "$(jq -er '.reason' "$backup_root/$manual_backup/manifest.json")" = manual-backup
api_request GET backups "" ""
test "$api_status" = 200
test "$(jq -r --arg checkpoint "$update_checkpoint" 'any(.recovery_points[]; .id == $checkpoint and (.reason | startswith("update-to-")))' <<< "$api_body")" = true
record_check manual-backup "Later manual backup retained the pre-update checkpoint exposed for website rollback"

current_check=website-rollback
expect_api_error POST rollbacks "" "{\"recovery_point_id\":\"$update_checkpoint\"}" 403 mutation_authorization_required
next_totp rollback
rollback_code=$totp_code
website_expect_validation_error "" data-admin-errors system-updates/updater/rollback \
    "current_admin_password=$admin_password" "updater_authorization_code=$rollback_code" "recovery_point_id=$manual_backup"
docker exec geoflow-postgres-prod psql --username=geo_user --dbname=geo_flow --set ON_ERROR_STOP=1 \
    --command "DELETE FROM system_update_runs WHERE run_uuid='${marker}-recent'; INSERT INTO system_update_runs (run_uuid, action, status, current_version, target_version, created_at, updated_at) VALUES ('${marker}-post', 'apply', 'completed', '3.0.0', '3.0.0', NOW(), NOW()) ON CONFLICT (run_uuid) DO NOTHING" >/dev/null
printf 'after-update\n' > "$instance_root/storage/app/phase-c-rehearsal.txt"
printf 'after-update\n' > "$instance_root/storage/app/phase-c-post-update.txt"
python3 - "$instance_root/.env.prod" <<'PY'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
contents = path.read_text()
if "PHASE_C_REHEARSAL_MARKER=before-update" not in contents:
    raise SystemExit("pre-update environment marker is missing")
path.write_text(contents.replace("PHASE_C_REHEARSAL_MARKER=before-update", "PHASE_C_REHEARSAL_MARKER=after-update"))
PY
docker exec --env REDISCLI_AUTH="$redis_password" geoflow-redis-prod redis-cli SET "geoflow:${marker}:before" after-update >/dev/null 2>&1
docker exec --env REDISCLI_AUTH="$redis_password" geoflow-redis-prod redis-cli SET "geoflow:${marker}:post" after-update >/dev/null 2>&1
website_start_operation system-updates/updater/rollback \
    "current_admin_password=$admin_password" "updater_authorization_code=$rollback_code" "recovery_point_id=$update_checkpoint"
wait_for_operation website-rollback succeeded rollback
test "$(jq -er '.version' "$instance_root/version.json")" = 2.3.0
test "$(<"$instance_root/storage/app/phase-c-rehearsal.txt")" = before-update
test ! -e "$instance_root/storage/app/phase-c-post-update.txt"
grep -qx 'PHASE_C_REHEARSAL_MARKER=before-update' "$instance_root/.env.prod"
restored_pre_rows=$(docker exec geoflow-postgres-prod psql --username=geo_user --dbname=geo_flow --tuples-only --no-align --command "SELECT count(*) FROM system_update_runs WHERE run_uuid='${marker}-recent'")
restored_post_rows=$(docker exec geoflow-postgres-prod psql --username=geo_user --dbname=geo_flow --tuples-only --no-align --command "SELECT count(*) FROM system_update_runs WHERE run_uuid='${marker}-post'")
test "$restored_pre_rows" = 1
test "$restored_post_rows" = 0
test "$(docker exec --env REDISCLI_AUTH="$redis_password" geoflow-redis-prod redis-cli GET "geoflow:${marker}:before" 2>/dev/null)" = before-update
test "$(docker exec --env REDISCLI_AUTH="$redis_password" geoflow-redis-prod redis-cli EXISTS "geoflow:${marker}:post" 2>/dev/null)" = 0
verify_fpm_bridge_permissions "$evidence_dir/fpm-website-rollback.json"
record_check website-rollback "Website accepted only the newest pre-update checkpoint and restored database, Redis, storage, environment, deployment state, and version"

current_check=forced-migration-failure
prepare_restore_fixture forced-migration-failure
set_fault fail-migrate
next_totp update
start_mutation updates "$totp_code"
wait_for_operation forced-migration-failure rolled_back update
clear_fault
test "$(jq -r 'any(.stages[]; .name == "migrate" and .status == "failed")' "$evidence_dir/operation-forced-migration-failure.json")" = true
test "$(jq -r 'any(.stages[]; .name == "rolled_back" and .status == "succeeded")' "$evidence_dir/operation-forced-migration-failure.json")" = true
test "$(jq -er '.version' "$instance_root/version.json")" = 2.3.0
assert_restore_fixture
record_check forced-migration-failure "Injected migration failure restored PostgreSQL, Redis, storage, site environment, managed release state, Compose, and the Phase B version"

current_check=forced-activation-failure
prepare_restore_fixture forced-activation-failure
set_fault fail-activate
next_totp update
start_mutation updates "$totp_code"
wait_for_operation forced-activation-failure rolled_back update
clear_fault
test "$(jq -r 'any(.stages[]; .name == "activate" and .status == "failed")' "$evidence_dir/operation-forced-activation-failure.json")" = true
test "$(jq -r 'any(.stages[]; .name == "rolled_back" and .status == "succeeded")' "$evidence_dir/operation-forced-activation-failure.json")" = true
test "$(jq -er '.version' "$instance_root/version.json")" = 2.3.0
assert_restore_fixture
record_check forced-activation-failure "Removed the prepared deployment after migration; automatic recovery restored all data, configuration, deployment, and Phase B version surfaces"

current_check=restart-during-migration
prepare_restore_fixture restart-during-migration
set_fault pause-migrate
next_totp update
start_mutation updates "$totp_code"
wait_for_fault_marker migrate-completed
test "$(jq -er '.status' "$instance_dir/operations/current.json")" = running
test "$(jq -er '.current_stage' "$instance_dir/operations/current.json")" = migrate
systemctl kill --kill-who=all --signal=SIGKILL geoflow-updater.service
clear_fault
wait_for_service
wait_for_operation restart-during-migration rolled_back update
test "$(jq -er '.current_stage' "$evidence_dir/operation-restart-during-migration.json")" = reconciled
test "$(jq -er '.version' "$instance_root/version.json")" = 2.3.0
assert_restore_fixture
record_check restart-during-migration "The real migration and corruption completed while its stage remained durable-running; SIGKILL reconciled every protected surface to one rolled-back operation"

current_check=persistent-recovery
prepare_restore_fixture persistent-recovery
set_fault pause-migrate-fail-restore
next_totp update
start_mutation updates "$totp_code"
wait_for_fault_marker migrate-completed
test "$(jq -er '.status' "$instance_dir/operations/current.json")" = running
test "$(jq -er '.current_stage' "$instance_dir/operations/current.json")" = migrate
systemctl kill --kill-who=all --signal=SIGKILL geoflow-updater.service
wait_for_service
wait_for_operation_status recovery_required
test "$(jq -er '.reconcile_attempts' "$evidence_dir/operation-recovery-required.json")" -ge 1
test -n "$(jq -r '.next_reconcile_at // empty' "$evidence_dir/operation-recovery-required.json")"
api_request GET status "" ""
test "$api_status" = 200
future_counter=$(( $(date +%s) / 30 + 1 ))
blocked_code=$(generate_totp_for_secret "${factor_secrets[update]}" "$future_counter")
mask_value "$blocked_code"
expect_api_error POST updates "$blocked_code" "" 409 operation_active
reconcile_attempts=$(jq -er '.reconcile_attempts' "$evidence_dir/operation-recovery-required.json")
sleep 6
api_request GET operations/current "" ""
test "$(jq -er '.reconcile_attempts' <<< "$api_body")" = "$reconcile_attempts"
clear_fault
wait_for_operation recovered-after-fault rolled_back update
test "$(jq -er '.current_stage' "$evidence_dir/operation-recovered-after-fault.json")" = reconciled
test "$(jq -er '.reconcile_attempts' "$evidence_dir/operation-recovered-after-fault.json")" = 0
test "$(jq -er '.version' "$instance_root/version.json")" = 2.3.0
assert_restore_fixture
record_check persistent-recovery "Failed startup restore persisted recovery_required, kept status available, blocked mutations, honored backoff, and later restored every protected surface"

current_check=restart-during-resume
set_fault pause-resume
next_totp update
start_mutation updates "$totp_code"
wait_for_fault_marker resume-completed
test "$(jq -er '.status' "$instance_dir/operations/current.json")" = running
test "$(jq -er '.current_stage' "$instance_dir/operations/current.json")" = resume
systemctl kill --kill-who=all --signal=SIGKILL geoflow-updater.service
clear_fault
wait_for_service
wait_for_operation restart-during-resume succeeded update
test "$(jq -er '.current_stage' "$evidence_dir/operation-restart-during-resume.json")" = reconciled
test "$(jq -er '.version' "$instance_root/version.json")" = 3.0.0
geoflow-updater doctor --instance primary --json > "$evidence_dir/doctor-final.json"
test "$(jq -r '.status' "$evidence_dir/doctor-final.json")" = pass
verify_restarted_website_bridge resume phase-c
record_check restart-during-resume "The real resume completed while its stage remained durable-running; SIGKILL before command return reconciled the activated candidate to healthy succeeded"

current_check=anti-spray
current_counter=$(( $(date +%s) / 30 ))
valid_update=$(generate_totp_for_secret "${factor_secrets[update]}" "$current_counter")
valid_backup=$(generate_totp_for_secret "${factor_secrets[backup]}" "$current_counter")
mask_value "$valid_update"
mask_value "$valid_backup"
random_hex=$(openssl rand -hex 4)
invalid_code=000000
for ((candidate_number = 0; candidate_number < 10; candidate_number++)); do
    printf -v candidate_code '%06d' "$(( (16#$random_hex + candidate_number) % 1000000 ))"
    if [[ $candidate_code != "$valid_update" && $candidate_code != "$valid_backup" ]]; then
        invalid_code=$candidate_code
        break
    fi
done
mask_value "$invalid_code"
for attempt in 1 2 3 4; do
    if (( attempt % 2 == 1 )); then
        expect_api_error POST updates "$invalid_code" "" 403 mutation_authorization_invalid
    else
        expect_api_error POST backups "$invalid_code" "" 403 mutation_authorization_invalid
    fi
done
expect_api_error POST backups "$invalid_code" "" 429 mutation_authorization_rate_limited
systemctl restart geoflow-updater.service
systemctl is-active --quiet geoflow-updater.service
expect_api_error POST updates "$valid_update" "" 429 mutation_authorization_rate_limited
verify_restarted_website_bridge anti-spray phase-c
awk '{print "failures=" $1 " locked=yes"}' "$instance_dir/mutation.attempts" > "$evidence_dir/anti-spray-state.txt"
record_check anti-spray "Five distributed failures triggered a persisted 15-minute lockout that survived service restart; native tests cover expiry and scoped clearing"

current_check=secret-free-logs
scan_runtime_logs
record_check secret-free-logs "Updater journal, container output, and bounded Laravel daily logs contain none of the generated credentials, session tokens, control token, factor URIs, secrets, or accepted codes"

current_check=complete
result_status=pass
# Machine evidence concludes with {"result":"pass"} only after every check above succeeds.
log "Native Phase C rehearsal passed for $PLATFORM"
