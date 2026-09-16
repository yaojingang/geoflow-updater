<?php

declare(strict_types=1);

/** Fixed adapter for the pinned Core 3.1.0 image. Only the host selects this program. */
final class GeoFlowLegacyRecoveryAdapter
{
    public const ROOT = '/var/www/html';
    public const CONTROL = '/run/geoflow-recovery-control/state.json';
    public const INTENTS = [
        'jobs', 'failed_jobs', 'manual_publications', 'job_batches', 'site_theme_replications', 'ai_workspace_steps',
        'ai_workspace_external_operations', 'task_runs', 'article_distributions', 'tasks', 'url_import_jobs',
        'ai_workspace_runs', 'article_ai_quality_checks', 'article_ai_optimization_runs', 'title_generation_runs',
        'knowledge_fact_generation_runs', 'ai_visibility_runs', 'enterprise_knowledge_projects', 'knowledge_bases',
        'hosted_site_allocation_requests', 'hosted_site_article_assignments',
    ];
    private const PREPARATIONS = 'geoflow_legacy_recovery_preparations';
    private const QUARANTINES = 'geoflow_legacy_recovery_quarantines';

    public function __construct(private readonly PDO $db, private readonly string $control = self::CONTROL)
    {
        $db->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
    }

    public static function state(string $path = self::CONTROL): array
    {
        clearstatcache(true, $path);
        if (is_link(dirname($path)) || is_link($path) || !is_file($path)) {
            throw new RuntimeException('recovery_state_unavailable');
        }
        $stream = @fopen($path, 'rb');
        if ($stream === false) {
            throw new RuntimeException('recovery_state_unavailable');
        }
        try {
            $opened = fstat($stream);
            $current = lstat($path);
            if (!$opened || !$current || ($opened['mode'] & 0170000) !== 0100000 || $opened['ino'] !== $current['ino'] || $opened['dev'] !== $current['dev']) {
                throw new RuntimeException('recovery_state_unavailable');
            }
            $bytes = stream_get_contents($stream, 8193);
            if (!is_string($bytes) || strlen($bytes) > 8192) {
                throw new RuntimeException('recovery_state_unavailable');
            }
            $state = json_decode($bytes, true, 8, JSON_THROW_ON_ERROR);
        } finally {
            fclose($stream);
        }
        if (!is_array($state) || ($state['schema_version'] ?? null) !== 1 || ($state['instance_id'] ?? null) !== 'primary'
            || ($state['minimum_updater_protocol'] ?? null) !== 5 || !self::hex($state['host_id'] ?? null, 32)
            || !self::hex($state['epoch'] ?? null, 32) || !in_array($state['phase'] ?? null, ['ready', 'restoring', 'validating', 'http_ready'], true)
            || !array_key_exists('transaction_id', $state)
            || !(($state['phase'] === 'ready' && $state['transaction_id'] === null)
                || ($state['phase'] !== 'ready' && is_string($state['transaction_id']) && preg_match('/\A[A-Za-z0-9][A-Za-z0-9._-]{7,127}\z/', $state['transaction_id']) === 1))) {
            throw new RuntimeException('recovery_state_unavailable');
        }

        return $state;
    }

    public function inspect(): array
    {
        $this->assertSchema();

        return ['schema_version' => 1, 'status' => 'pass', 'admin_digest' => $this->adminDigest(), 'theme_revisions' => 0];
    }

    public function prepare(string $transaction, string $expected): array
    {
        $state = $this->boundary($transaction);
        if (!self::hex($expected, 64)) {
            throw new RuntimeException('recovery_admin_review_required');
        }
        $this->assertSchema();
        $this->db->beginTransaction();
        try {
            if ($this->db->getAttribute(PDO::ATTR_DRIVER_NAME) === 'pgsql') {
                $this->db->query('SELECT id FROM admins ORDER BY id FOR UPDATE')->fetchAll();
            }
            $this->db->exec('CREATE TABLE IF NOT EXISTS '.self::PREPARATIONS.' (epoch VARCHAR(32) PRIMARY KEY, transaction_id VARCHAR(128) NOT NULL, admin_digest_before VARCHAR(64) NOT NULL, admin_digest_after VARCHAR(64) NOT NULL, report TEXT NOT NULL)');
            $this->db->exec('CREATE TABLE IF NOT EXISTS '.self::QUARANTINES.' (epoch VARCHAR(32) NOT NULL, source_table VARCHAR(80) NOT NULL, source_id VARCHAR(128) NOT NULL, source_sha256 VARCHAR(64) NOT NULL, status VARCHAR(30) NOT NULL, PRIMARY KEY (epoch, source_table, source_id))');
            $prior = $this->row('SELECT * FROM '.self::PREPARATIONS.' WHERE epoch = ?', [$state['epoch']]);
            if ($prior !== null) {
                if ($prior['transaction_id'] !== $transaction || !hash_equals($prior['admin_digest_before'], $expected)) {
                    throw new RuntimeException('recovery_preparation_identity_conflict');
                }
                $report = $this->verify($transaction);
                $this->db->commit();

                return $report;
            }
            if (!hash_equals($expected, $this->adminDigest())) {
                throw new RuntimeException('recovery_admin_review_required');
            }
            $count = 0;
            $insert = $this->db->prepare('INSERT INTO '.self::QUARANTINES.' (epoch, source_table, source_id, source_sha256, status) VALUES (?, ?, ?, ?, ?)');
            foreach (self::INTENTS as $table) {
                foreach ($this->db->query('SELECT * FROM "'.$table.'" ORDER BY id') as $source) {
                    $source = array_filter($source, 'is_string', ARRAY_FILTER_USE_KEY);
                    $insert->execute([$state['epoch'], $table, (string)$source['id'], $this->sourceDigest($source), 'held']);
                    $count++;
                }
            }
            $this->db->exec('DELETE FROM personal_access_tokens');
            $this->db->exec('UPDATE admins SET remember_token = NULL, auth_version = auth_version + 1');
            $report = ['schema_version' => 1, 'status' => 'pass', 'transaction_id' => $transaction, 'epoch' => $state['epoch'],
                'credentials_invalidated' => true, 'administrators_verified' => true, 'theme_revisions' => 0,
                'background_status' => 'held', 'quarantine_count' => $count, 'quarantine_sha256' => $this->quarantineDigest($state['epoch'], true)];
            $this->db->prepare('INSERT INTO '.self::PREPARATIONS.' (epoch, transaction_id, admin_digest_before, admin_digest_after, report) VALUES (?, ?, ?, ?, ?)')
                ->execute([$state['epoch'], $transaction, $expected, $this->adminDigest(), json_encode($report, JSON_THROW_ON_ERROR)]);
            $this->boundary($transaction);
            $this->db->commit();

            return $report;
        } catch (Throwable $exception) {
            if ($this->db->inTransaction()) {
                $this->db->rollBack();
            }
            throw $exception;
        }
    }

    public function verify(string $transaction): array
    {
        $state = $this->boundary($transaction);
        $this->assertSchema();
        $prior = $this->row('SELECT * FROM '.self::PREPARATIONS.' WHERE epoch = ?', [$state['epoch']]);
        if ($prior === null || $prior['transaction_id'] !== $transaction || !hash_equals($prior['admin_digest_after'], $this->adminDigest())
            || $this->db->query('SELECT COUNT(*) FROM personal_access_tokens')->fetchColumn() != 0
            || $this->db->query('SELECT COUNT(*) FROM admins WHERE remember_token IS NOT NULL')->fetchColumn() != 0) {
            throw new RuntimeException('recovery_preparation_unverified');
        }
        $report = json_decode($prior['report'], true, 16, JSON_THROW_ON_ERROR);
        if (!hash_equals($report['quarantine_sha256'], $this->quarantineDigest($state['epoch'], true))
            || (int)$this->row('SELECT COUNT(*) AS total FROM '.self::QUARANTINES.' WHERE epoch = ?', [$state['epoch']])['total'] !== $report['quarantine_count']) {
            throw new RuntimeException('recovery_quarantine_changed');
        }
        $this->boundary($transaction);

        return $report;
    }

    private function boundary(string $transaction): array
    {
        $state = self::state($this->control);
        if ($state['phase'] !== 'validating' || $state['transaction_id'] !== $transaction) {
            throw new RuntimeException('recovery_prepare_boundary_invalid');
        }

        return $state;
    }

    private function assertSchema(): void
    {
        foreach (array_merge(['admins', 'personal_access_tokens'], self::INTENTS) as $table) {
            $this->db->query('SELECT id FROM "'.$table.'" LIMIT 0');
        }
        $driver = $this->db->getAttribute(PDO::ATTR_DRIVER_NAME);
        $newThemeTable = $driver === 'pgsql'
            ? $this->db->query("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name IN ('theme_workspaces', 'theme_revisions')")->fetchColumn()
            : $this->db->query("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('theme_workspaces', 'theme_revisions')")->fetchColumn();
        if ((int)$newThemeTable !== 0) {
            throw new RuntimeException('recovery_legacy_theme_schema_unsupported');
        }
    }

    private function adminDigest(): string
    {
        $hash = hash_init('sha256');
        $statement = $this->db->query('SELECT id, username, email, password, role, status, auth_version, created_at FROM admins ORDER BY id');
        while ($admin = $statement->fetch(PDO::FETCH_ASSOC)) {
            hash_update($hash, json_encode($admin, JSON_THROW_ON_ERROR)."\n");
        }

        return hash_final($hash);
    }

    private function sourceDigest(array $source): string
    {
        ksort($source);

        return hash('sha256', json_encode($source, JSON_THROW_ON_ERROR));
    }

    private function quarantineDigest(string $epoch, bool $verify): string
    {
        $hash = hash_init('sha256');
        if ($verify) {
            foreach (self::INTENTS as $table) {
                $actual = (int)$this->db->query('SELECT COUNT(*) FROM "'.$table.'"')->fetchColumn();
                $expected = $this->row('SELECT COUNT(*) AS total FROM '.self::QUARANTINES.' WHERE epoch = ? AND source_table = ?', [$epoch, $table]);
                if ($actual !== (int)$expected['total']) {
                    throw new RuntimeException('recovery_quarantine_changed');
                }
            }
        }
        $statement = $this->db->prepare('SELECT source_table, source_id, source_sha256, status FROM '.self::QUARANTINES.' WHERE epoch = ? ORDER BY source_table, source_id');
        $statement->execute([$epoch]);
        while ($entry = $statement->fetch(PDO::FETCH_ASSOC)) {
            if (!in_array($entry['source_table'], self::INTENTS, true) || $entry['status'] !== 'held') {
                throw new RuntimeException('recovery_quarantine_changed');
            }
            if ($verify) {
                $source = $this->row('SELECT * FROM "'.$entry['source_table'].'" WHERE id = ?', [$entry['source_id']]);
                if ($source === null || !hash_equals($entry['source_sha256'], $this->sourceDigest($source))) {
                    throw new RuntimeException('recovery_quarantine_changed');
                }
            }
            hash_update($hash, json_encode(array_values($entry), JSON_THROW_ON_ERROR)."\n");
        }

        return hash_final($hash);
    }

    private function row(string $sql, array $bindings): ?array
    {
        $statement = $this->db->prepare($sql);
        $statement->execute($bindings);
        $row = $statement->fetch(PDO::FETCH_ASSOC);

        return $row === false ? null : $row;
    }

    private static function hex(mixed $value, int $length): bool
    {
        return is_string($value) && preg_match('/\A[a-f0-9]{'.$length.'}\z/', $value) === 1;
    }
}

if (realpath((string)($_SERVER['SCRIPT_FILENAME'] ?? '')) === __FILE__) {
    try {
        $input = [];
        foreach (array_slice($argv, 1) as $argument) {
            if (in_array($argument, ['--json', '--no-interaction'], true)) {
                continue;
            }
            if (!preg_match('/\A--(phase|transaction|expected-admin-digest)=(.+)\z/', $argument, $match) || isset($input[$match[1]])) {
                throw new RuntimeException('recovery_arguments_invalid');
            }
            $input[$match[1]] = $match[2];
        }
        if (!in_array($input['phase'] ?? null, ['inspect', 'prepare', 'verify'], true)
            || !in_array('--json', $argv, true)
            || ($input['phase'] !== 'inspect' && preg_match('/\A[A-Za-z0-9][A-Za-z0-9._-]{7,127}\z/', $input['transaction'] ?? '') !== 1)) {
            throw new RuntimeException('recovery_arguments_invalid');
        }
        if (hash_file('sha256', GeoFlowLegacyRecoveryAdapter::ROOT.'/bootstrap/app.php') !== '92fbc3017365ca1d0f0a3703e865ae9c03bd2cfbf977db7e6ac421eb65fb8a67') {
            throw new RuntimeException('recovery_legacy_build_unsupported');
        }
        require GeoFlowLegacyRecoveryAdapter::ROOT.'/vendor/autoload.php';
        $app = require GeoFlowLegacyRecoveryAdapter::ROOT.'/bootstrap/app.php';
        $app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();
        $pdo = $app->make('db')->connection()->getPdo();
        if ($pdo->getAttribute(PDO::ATTR_DRIVER_NAME) !== 'pgsql') {
            throw new RuntimeException('recovery_legacy_database_unsupported');
        }
        $adapter = new GeoFlowLegacyRecoveryAdapter($pdo);
        $report = match ($input['phase']) {
            'inspect' => $adapter->inspect(),
            'prepare' => $adapter->prepare($input['transaction'], $input['expected-admin-digest'] ?? ''),
            'verify' => $adapter->verify($input['transaction']),
        };
        fwrite(STDOUT, json_encode($report, JSON_THROW_ON_ERROR)."\n");
    } catch (Throwable $exception) {
        $code = preg_match('/\Arecovery_[a-z_]+\z/', $exception->getMessage()) ? $exception->getMessage() : 'recovery_validation_failed';
        fwrite(STDOUT, json_encode(['schema_version' => 1, 'status' => 'fail', 'error' => $code], JSON_THROW_ON_ERROR)."\n");
        exit(1);
    }
}
