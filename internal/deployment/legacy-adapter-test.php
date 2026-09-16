<?php

declare(strict_types=1);

require __DIR__.'/legacy-recovery.php';
require __DIR__.'/legacy-http-guard.php';

$directory = sys_get_temp_dir().'/geoflow-legacy-adapter-'.bin2hex(random_bytes(8));
mkdir($directory, 0700);
$state = ['schema_version' => 1, 'instance_id' => 'primary', 'host_id' => str_repeat('b', 32), 'epoch' => str_repeat('a', 32), 'transaction_id' => 'restore-test-123', 'phase' => 'validating', 'minimum_updater_protocol' => 5];
$checkCount = 0;
$check = static function (bool $condition, string $message) use (&$checkCount): void {
    $checkCount++;
    if (!$condition) {
        throw new RuntimeException($message);
    }
};
$reject = static function (callable $operation, string $message) use ($check): void {
    try {
        $operation();
    } catch (Throwable) {
        $check(true, $message);
        return;
    }
    $check(false, $message);
};
try {
    file_put_contents($directory.'/state.json', json_encode($state));
    $pdo = new PDO('sqlite::memory:');
    $pdo->exec('CREATE TABLE admins (id INTEGER PRIMARY KEY, username TEXT, email TEXT, password TEXT, role TEXT, status TEXT, auth_version INTEGER, created_at TEXT, remember_token TEXT)');
    $pdo->exec("INSERT INTO admins VALUES (1, 'reviewed-admin', 'admin@example.invalid', 'fixture-password-hash', 'super_admin', 'active', 7, '2026-09-01 00:00:00', 'old-remember')");
    $pdo->exec('CREATE TABLE personal_access_tokens (id INTEGER PRIMARY KEY, token TEXT)');
    $pdo->exec("INSERT INTO personal_access_tokens VALUES (1, 'restored-token')");
    foreach (GeoFlowLegacyRecoveryAdapter::INTENTS as $table) {
        $pdo->exec('CREATE TABLE "'.$table.'" (id INTEGER PRIMARY KEY, status TEXT, payload TEXT)');
    }
    $pdo->exec("INSERT INTO jobs VALUES (1, 'pending', 'sensitive-original-payload')");
    $adapter = new GeoFlowLegacyRecoveryAdapter($pdo, $directory.'/state.json');
    $inspection = $adapter->inspect();
    $check($inspection['theme_revisions'] === 0 && strlen($inspection['admin_digest']) === 64, 'inspect digest');
    $check(!str_contains(json_encode($inspection), 'fixture-password-hash'), 'inspect must not leak individual credentials');
    $reject(fn () => $adapter->prepare('restore-test-123', str_repeat('d', 64)), 'unreviewed admin snapshot accepted');
    $check($pdo->query('SELECT COUNT(*) FROM personal_access_tokens')->fetchColumn() == 1, 'mismatch modified credentials');
    $report = $adapter->prepare('restore-test-123', $inspection['admin_digest']);
    $check($report === $adapter->prepare('restore-test-123', $inspection['admin_digest']), 'prepare is not idempotent');
    $check($report === $adapter->verify('restore-test-123'), 'verify report differs');
    $check($pdo->query('SELECT auth_version FROM admins')->fetchColumn() == 8, 'auth version incremented more than once');
    $check($pdo->query('SELECT COUNT(*) FROM personal_access_tokens')->fetchColumn() == 0, 'old tokens remain');
    $check($pdo->query('SELECT remember_token FROM admins')->fetchColumn() === null, 'remember token remains');
    $check($pdo->query('SELECT payload FROM jobs')->fetchColumn() === 'sensitive-original-payload', 'source payload was removed');
    $check($report['quarantine_count'] === 1 && !str_contains(json_encode($report), 'sensitive-original-payload'), 'quarantine receipt privacy');
    $pdo->exec("INSERT INTO jobs VALUES (2, 'pending', 'new')");
    $reject(fn () => $adapter->verify('restore-test-123'), 'new source identity was not detected');
    $pdo->exec('DELETE FROM jobs WHERE id = 2');
    $pdo->exec("UPDATE jobs SET payload = 'changed' WHERE id = 1");
    $reject(fn () => $adapter->verify('restore-test-123'), 'changed source payload was not detected');
    $pdo->exec("UPDATE jobs SET payload = 'sensitive-original-payload' WHERE id = 1");
    $pdo->exec("UPDATE admins SET role = 'admin'");
    $reject(fn () => $adapter->verify('restore-test-123'), 'changed administrator privileges were not detected');
    $pdo->exec("UPDATE admins SET role = 'super_admin'");
    $state['epoch'] = str_repeat('c', 32);
    file_put_contents($directory.'/state.json', json_encode($state));
    $reject(fn () => $adapter->verify('restore-test-123'), 'previous epoch receipt accepted');
    $state['epoch'] = str_repeat('a', 32);
    $state['phase'] = 'http_ready';
    file_put_contents($directory.'/state.json', json_encode($state));
    $reject(fn () => $adapter->prepare('restore-test-123', $inspection['admin_digest']), 'open traffic permitted destructive preparation');
    foreach ([['artisan', 'horizon'], ['artisan', 'queue:work', '--once'], ['artisan', 'schedule:run'], ['artisan', 'tinker'], ['/tmp/injected.php']] as $args) {
        $check(!GeoFlowLegacyHttpGuard::allowsCommand($args), 'unsafe command allowed');
    }
    foreach ([['artisan', 'up'], ['artisan', 'down'], ['/run/geoflow-recovery-adapter/legacy-recovery.php'], ['/run/geoflow-recovery-redis.php']] as $args) {
        $check(GeoFlowLegacyHttpGuard::allowsCommand($args), 'fixed host step blocked');
    }
    $check(GeoFlowLegacyHttpGuard::allowsRoute('GET', 'site.article', 'article/read'), 'read blocked');
    $check(!GeoFlowLegacyHttpGuard::allowsRoute('GET', 'admin.knowledge-bases.facts.index', 'geo_admin/knowledge-bases/1/facts'), 'lazy business creation GET allowed');
    $check(!GeoFlowLegacyHttpGuard::allowsRoute('HEAD', 'admin.knowledge-bases.facts.index', 'geo_admin/knowledge-bases/1/facts'), 'lazy business creation HEAD allowed');
    $check(!GeoFlowLegacyHttpGuard::allowsRoute('GET', 'unknown.route', 'future/read'), 'unverified GET allowed');
    $check(!GeoFlowLegacyHttpGuard::allowsRoute('GET', null, 'api/v1/manual-publications/1'), 'unverified API GET allowed');
    $check(GeoFlowLegacyHttpGuard::allowsRoute('POST', 'admin.login.attempt', 'private/login'), 'password login blocked');
    $check(GeoFlowLegacyHttpGuard::allowsRoute('POST', 'admin.logout', 'private/logout'), 'logout blocked');
    $check(!GeoFlowLegacyHttpGuard::allowsRoute('POST', null, 'api/v1/tasks/1/enqueue'), 'queue intent allowed');
    $check(!GeoFlowLegacyHttpGuard::allowsRoute('POST', null, 'api/v1/browser-operations/device-token'), 'restored approved pairing allowed');
    $check(!GeoFlowLegacyHttpGuard::allowsRoute('DELETE', null, 'api/v1/articles/1'), 'business deletion allowed');
    $check(!GeoFlowLegacyHttpGuard::allowsRoute('PATCH', null, 'admin/api-tokens/1'), 'token update allowed');
    $state['phase'] = 'ready';
    file_put_contents($directory.'/state.json', json_encode($state));
    $reject(fn () => GeoFlowLegacyRecoveryAdapter::state($directory.'/state.json'), 'ready state with restore transaction accepted');
    file_put_contents($directory.'/state.json', '{}');
    $reject(fn () => GeoFlowLegacyRecoveryAdapter::state($directory.'/state.json'), 'corrupt state accepted');
    echo json_encode(['status' => 'pass', 'assertions' => $checkCount])."\n";
} finally {
    unlink($directory.'/state.json');
    rmdir($directory);
}
