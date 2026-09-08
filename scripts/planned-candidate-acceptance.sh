#!/usr/bin/env bash
# Ephemeral candidate application contract acceptance; never targets an enrolled host.
set -Eeuo pipefail
[[ $# == 2 ]] || { printf '%s\n' 'Usage: planned-candidate-acceptance.sh CANDIDATE_DIRECTORY EVIDENCE_DIRECTORY' >&2; exit 2; }
candidate=$(cd -- "$1" && pwd -P)
evidence=$2
[[ -f "$candidate/candidate.json" ]]
mkdir -p -- "$evidence"
evidence=$(cd -- "$evidence" && pwd -P)
manifest=$candidate/targets-source/releases/current.json
[[ $(jq -er '.schema_version' "$manifest") == 3 ]]
version=$(jq -er '.version' "$manifest")
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]
app_image=$(jq -er '.app_image' "$manifest")
postgres_image=$(jq -er '.postgres_images["18"]' "$manifest")
redis_image=$(jq -er '.redis_images["8"]' "$manifest")
[[ $(sha256sum "$manifest" | cut -d ' ' -f 1) == $(jq -er '.targets.release_manifest_sha256' "$candidate/candidate.json") ]]
expected_plan=$(jq -er '.targets.upgrade_plan_sha256' "$candidate/candidate.json")
[[ $expected_plan =~ ^[a-f0-9]{64}$ ]]
plan=$candidate/targets-source/releases/$version/upgrade-plan.json
[[ $(sha256sum "$plan" | cut -d ' ' -f 1) == "$expected_plan" ]]
for image in "$app_image" "$postgres_image" "$redis_image"; do
    [[ $image =~ ^[a-zA-Z0-9./_-]+@sha256:[a-f0-9]{64}$ ]]
    docker pull "$image"
done
acceptance_root=$(mktemp -d)
name=geoflow-acceptance-$(date +%s)-$$
cleanup() {
    docker rm -f "$name-app" "$name-db" "$name-redis" >/dev/null 2>&1 || true
    docker network rm "$name" >/dev/null 2>&1 || true
    rm -f "$acceptance_root/environment"
    rmdir "$acceptance_root" 2>/dev/null || true
}
trap cleanup EXIT
app_exec() {
    docker exec --user 33:33 "$name-app" "$@"
}
check_report() {
    jq -e --arg version "$version" --arg hash "$expected_plan" --arg phase "$2" \
        '.schema_version == 1 and .status == "pass" and .phase == $phase and .version == $version and .plan_sha256 == $hash and (.pending_migrations | length == 0) and (.checks | length > 0)' "$1" >/dev/null
}
umask 077
password=$(openssl rand -hex 32)
app_key=$(openssl rand -base64 32)
cat > "$acceptance_root/environment" <<ENV
APP_ENV=production
APP_DEBUG=false
APP_KEY=base64:$app_key
APP_URL=https://acceptance.example.invalid
DB_CONNECTION=pgsql
DB_HOST=postgres
DB_DATABASE=geoflow
DB_USERNAME=geoflow
DB_PASSWORD=$password
REDIS_HOST=redis
REDIS_PASSWORD=null
CACHE_STORE=redis
QUEUE_CONNECTION=redis
SESSION_DRIVER=database
BROADCAST_CONNECTION=null
VIEW_COMPILED_PATH=/var/www/html/bootstrap/cache/views
GEOFLOW_ADMIN_USERNAME=acceptance
GEOFLOW_ADMIN_EMAIL=acceptance@example.invalid
GEOFLOW_ADMIN_PASSWORD=$password
GEOFLOW_INITIAL_ADMIN_HINT_ENABLED=false
GEOFLOW_SECURITY_FRESH_INSTALL_CONFIRMED=false
GEOFLOW_SECURITY_UPGRADE_DRAIN_CONFIRMED=true
AUTO_MIGRATE=false
AUTO_INSTALL_ONCE=false
AUTO_OPTIMIZE=false
AUTO_FIX_STORAGE_PERMISSIONS=false
ENV
docker network create "$name" >/dev/null
docker run -d --name "$name-db" --network "$name" --network-alias postgres -e POSTGRES_DB=geoflow -e POSTGRES_USER=geoflow -e "POSTGRES_PASSWORD=$password" "$postgres_image" >/dev/null
docker run -d --name "$name-redis" --network "$name" --network-alias redis "$redis_image" >/dev/null
docker run -d --name "$name-app" --user 33:33 --network "$name" --env-file "$acceptance_root/environment" --volume "$acceptance_root/environment:/var/www/html/.env:ro" --entrypoint sh "$app_image" -c 'set -eu; mkdir -p storage/app/private storage/app/public storage/framework/cache/data storage/framework/sessions storage/logs bootstrap/cache/views; exec sleep 7200' >/dev/null
for _ in $(seq 1 60); do
    if docker exec "$name-db" pg_isready -U geoflow -d geoflow >/dev/null 2>&1; then break; fi
    sleep 1
done
docker exec "$name-db" pg_isready -U geoflow -d geoflow >/dev/null
[[ $(app_exec id -u) == 33 && $(app_exec id -g) == 33 ]]
app_exec id > "$evidence/runtime-identity.txt"
[[ $(app_exec sha256sum deployment/upgrade-plan.json | cut -d ' ' -f 1) == "$expected_plan" ]]
[[ $(app_exec php -r 'echo json_decode(file_get_contents("version.json"), true)["version"];') == "$version" ]]
app_exec test ! -r /var/www/html/.env
app_exec /usr/local/bin/geoflow-entrypoint-prod php artisan about --no-interaction > "$evidence/entrypoint.txt"
docker exec --user 33:33 -e GEOFLOW_SECURITY_FRESH_INSTALL_CONFIRMED=true "$name-app" php artisan migrate --force --no-interaction > "$evidence/migrate.txt"
app_exec php artisan geoflow:install --no-interaction > "$evidence/install.txt"
app_exec php artisan geoflow:sync-system-knowledge --media --no-interaction > "$evidence/system-knowledge.txt"
app_exec php artisan geoflow:backfill-ai-quality-retrieval --json --no-interaction > "$evidence/retrieval.json"
app_exec php artisan geoflow:managed-images:readiness --json --no-interaction > "$evidence/managed-images.json"
for command in config:cache route:cache view:cache; do
    app_exec php artisan "$command" --no-interaction > "$evidence/${command/:/-}.txt"
done
app_exec php artisan geoflow:upgrade --phase=verify --plan-sha256="$expected_plan" --json --no-interaction > "$evidence/readiness.json"
check_report "$evidence/readiness.json" verify
# The second invocation verifies first-install idempotency on the same database.
app_exec php artisan geoflow:install --no-interaction > "$evidence/install-repeat.txt"

# Exercise the signed operation contract against the initialized disposable database.
# Maintenance mode provides the same stopped-service precondition as fresh installation.
# PHP must read the exact integer value without shell variable expansion.
# shellcheck disable=SC2016
source_sequence=$(app_exec php -r '$plan = json_decode(file_get_contents("deployment/upgrade-plan.json"), true, 32, JSON_THROW_ON_ERROR); $source = $plan["allowed_sources"][0] ?? 1; if (!is_int($source) || $source < 1) { exit(1); } echo $source;')
[[ $source_sequence =~ ^[1-9][0-9]*$ ]]
operation=$name
journal=/var/www/html/storage/framework/geoflow-upgrades/$operation.json
app_exec test ! -e "$journal"
for phase in inspect apply verify; do
    app_exec php artisan geoflow:upgrade --phase="$phase" --strategy=maintenance --source-sequence="$source_sequence" --operation="$operation" --plan-sha256="$expected_plan" --json --no-interaction > "$evidence/upgrade-$phase.json"
    check_report "$evidence/upgrade-$phase.json" "$phase"
    if [[ $phase == inspect ]]; then
        app_exec test ! -e "$journal"
    else
        jq -e '.checks.journal.status == "pass"' "$evidence/upgrade-$phase.json" >/dev/null
    fi
done
app_exec cat "$journal" > "$evidence/upgrade-journal.json"
jq -e --arg operation "$operation" --arg version "$version" --arg hash "$expected_plan" --argjson source "$source_sequence" --slurpfile plan "$plan" \
    '.schema_version == 1 and .status == "completed" and .operation == $operation and .plan_sha256 == $hash and .version == $version and .strategy == "maintenance" and .source_sequence == $source and .pending_migrations == [] and ((.steps | keys | sort) == ($plan[0].steps | map(.id) | sort)) and all(.steps[]; .status == "completed" and (.started_at | type == "string") and (.completed_at | type == "string"))' "$evidence/upgrade-journal.json" >/dev/null
journal_sha=$(app_exec sha256sum "$journal" | cut -d ' ' -f 1)
app_exec php artisan geoflow:upgrade --phase=apply --strategy=maintenance --source-sequence="$source_sequence" --operation="$operation" --plan-sha256="$expected_plan" --json --no-interaction > "$evidence/upgrade-apply-repeat.json"
check_report "$evidence/upgrade-apply-repeat.json" apply
jq -e '.checks.journal.status == "pass"' "$evidence/upgrade-apply-repeat.json" >/dev/null
[[ $(app_exec sha256sum "$journal" | cut -d ' ' -f 1) == "$journal_sha" ]]
printf '%s\n' 'Candidate image identity, UID 33 runtime, fresh database migrations, initialization, backfills, private cache compilation, standalone readiness and signed inspect/apply/verify/repeated-apply journal contracts passed. Installed-host upgrade and crash recovery require separate rehearsal.' > "$evidence/scope.txt"
