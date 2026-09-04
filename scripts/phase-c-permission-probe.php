<?php
// Diagnostic branch only: numeric permissions and bounded read-only bridge results.
ini_set('display_errors', '0');
ini_set('log_errors', '0');
$result = ['sapi' => PHP_SAPI];
foreach (['self', '1'] as $pid) {
    preg_match_all('/^(?:Name|Uid|Gid|Groups|CapEff):[^\n]*$/m', (string) @file_get_contents("/proc/$pid/status"), $matches);
    $result['process'][$pid] = $matches[0];
}
foreach (['/run', '/run/secrets', '/run/secrets/geoflow-updater-control-token', '/run/geoflow-updater', '/run/geoflow-updater/geoflow-updater.sock'] as $path) {
    $stat = @lstat($path);
    $result['paths'][$path] = $stat === false ? ['stat' => false] : [
        'mode' => sprintf('%04o', $stat['mode'] & 07777),
        'uid' => $stat['uid'], 'gid' => $stat['gid'], 'readable' => is_readable($path),
    ];
}
$errno = 0;
$error = '';
$socket = @stream_socket_client('unix:///run/geoflow-updater/geoflow-updater.sock', $errno, $error, 1);
$result['socket'] = ['connected' => is_resource($socket), 'errno' => $errno];
if (is_resource($socket)) {
    fclose($socket);
}
try {
    require '/var/www/html/vendor/autoload.php';
    $app = require '/var/www/html/bootstrap/app.php';
    $app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();
    $result['configured_paths_match'] = config('geoflow.updater_socket') === '/run/geoflow-updater/geoflow-updater.sock'
        && config('geoflow.updater_control_token_file') === '/run/secrets/geoflow-updater-control-token';
    $status = $app->make(App\Contracts\SystemUpdater\AgentClient::class)->status();
    $result['bridge'] = ['ok' => true, 'status' => $status['status']];
} catch (Throwable $exception) {
    $message = $exception->getMessage();
    $allowed = ['Updater control credential is unavailable.', 'Updater control credential is invalid.', 'Updater agent is not reachable.'];
    $result['bridge'] = ['ok' => false, 'class' => get_class($exception),
        'message' => in_array($message, $allowed, true) ? $message : '[message omitted: outside diagnostic allowlist]'];
}
header('Content-Type: application/json');
echo json_encode($result, JSON_THROW_ON_ERROR | JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES), "\n";
