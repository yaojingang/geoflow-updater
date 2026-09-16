<?php

namespace Tests\Feature;

use App\Models\Admin;
use Illuminate\Support\Facades\DB;
use Tests\TestCase;

// Run only against pinned Core source 6c963783bbf49f0b5ec9d0121a924ee54005b60d in an isolated test checkout.
$adapterDirectory = getenv('GEOFLOW_LEGACY_ADAPTER_DIRECTORY');
if (! is_string($adapterDirectory) || ! str_starts_with($adapterDirectory, '/')) {
    throw new \RuntimeException('An absolute test adapter directory is required.');
}
require_once $adapterDirectory.'/legacy-recovery.php';
require_once $adapterDirectory.'/legacy-http-guard.php';

class LegacyRecoveryAdapterIntegrationTest extends TestCase
{
    private string $directory;

    protected function setUp(): void
    {
        parent::setUp();
        $this->artisan('migrate:fresh', ['--force' => true])->run();
        $this->withoutVite();
        $this->directory = sys_get_temp_dir().'/geoflow-legacy-http-'.bin2hex(random_bytes(6));
        mkdir($this->directory, 0700);
        $this->state('http_ready');
        $this->app->instance(\GeoFlowLegacyHttpGuard::class, new \GeoFlowLegacyHttpGuard($this->directory.'/state.json'));
        \GeoFlowLegacyHttpGuard::install($this->app);
    }

    protected function tearDown(): void
    {
        unlink($this->directory.'/state.json');
        rmdir($this->directory);
        parent::tearDown();
    }

    private function state(string $phase): void
    {
        file_put_contents($this->directory.'/state.json', json_encode(['schema_version' => 1, 'instance_id' => 'primary', 'host_id' => str_repeat('b', 32), 'epoch' => str_repeat('a', 32), 'transaction_id' => 'legacy-restore-001', 'phase' => $phase, 'minimum_updater_protocol' => 5]));
    }

    private function admin(): Admin
    {
        return Admin::query()->create(['username' => 'legacy-recovery-admin', 'password' => 'secret-test-password', 'role' => 'super_admin', 'status' => 'active']);
    }

    public function test_real_old_schema_preparation_is_idempotent_and_invalidates_restored_credentials(): void
    {
        $this->state('validating');
        $admin = $this->admin();
        $admin->createToken('restored', ['*']);
        $admin->update(['remember_token' => 'restored-remember']);
        $adapter = new \GeoFlowLegacyRecoveryAdapter(DB::connection()->getPdo(), $this->directory.'/state.json');
        $inspection = $adapter->inspect();
        $report = $adapter->prepare('legacy-restore-001', $inspection['admin_digest']);
        $this->assertSame($report, $adapter->prepare('legacy-restore-001', $inspection['admin_digest']));
        $this->assertSame($report, $adapter->verify('legacy-restore-001'));
        $this->assertSame(0, $admin->tokens()->count());
        $this->assertNull($admin->fresh()->remember_token);
        $this->assertSame(2, $admin->fresh()->auth_version);
    }

    public function test_password_login_binds_new_session_and_cannot_issue_remember_credentials(): void
    {
        $admin = $this->admin();
        $this->post(route('admin.login.attempt'), ['username' => $admin->username, 'password' => 'secret-test-password', 'remember' => true])
            ->assertRedirect(route('admin.dashboard'))->assertSessionHas('admin_recovery_epoch', str_repeat('a', 32));
        $this->assertNull($admin->fresh()->remember_token);
        $this->get(route('admin.dashboard'))->assertOk();
        $this->post(route('admin.logout'))->assertRedirect(route('admin.login'));
    }

    public function test_old_session_with_colliding_auth_version_cannot_survive_recovery(): void
    {
        $admin = $this->admin();
        $this->actingAs($admin, 'admin')->withSession([Admin::AUTH_VERSION_SESSION_KEY => $admin->auth_version])
            ->get(route('admin.dashboard'))->assertRedirect(route('admin.login'));
    }

    public function test_facts_get_and_head_cannot_create_a_library_in_a_restored_database(): void
    {
        $admin = $this->admin();
        $base = \App\Models\KnowledgeBase::query()->create(['name' => 'Recovered knowledge', 'content' => 'Recovered evidence']);
        $this->actingAs($admin, 'admin')->withSession([Admin::AUTH_VERSION_SESSION_KEY => $admin->auth_version,
            \GeoFlowLegacyHttpGuard::SESSION_EPOCH => str_repeat('a', 32)]);
        $this->get(route('admin.knowledge-bases.facts.index', ['knowledgeBaseId' => $base->id]))->assertServiceUnavailable();
        $this->head(route('admin.knowledge-bases.facts.index', ['knowledgeBaseId' => $base->id]))->assertServiceUnavailable();
        $this->assertDatabaseCount('knowledge_fact_libraries', 0);
    }

    public function test_public_article_remains_readable_without_incrementing_views_or_analytics(): void
    {
        $category = \App\Models\Category::query()->create(['name' => 'Recovered category', 'slug' => 'recovered']);
        $author = \App\Models\Author::query()->create(['name' => 'Recovered author']);
        $article = \App\Models\Article::query()->create(['title' => 'Recovered article', 'slug' => 'recovered-article', 'content' => 'Recovered content',
            'category_id' => $category->id, 'author_id' => $author->id, 'status' => 'published', 'review_status' => 'approved',
            'published_at' => now()->subDay(), 'view_count' => 12]);
        $this->get(route('site.article', ['slug' => $article->slug]))->assertOk()->assertSee('Recovered content');
        $this->get(route('site.home'))->assertOk();
        $this->assertSame(12, $article->fresh()->view_count);
        $this->assertDatabaseCount('view_logs', 0);
    }

    public function test_business_writes_and_restored_browser_pairing_are_held_but_reads_work(): void
    {
        $admin = $this->admin();
        $token = $admin->createToken('new-read-only', ['*'])->plainTextToken;
        $this->get('/')->assertOk();
        $this->withToken($token)->postJson('/api/v1/tasks', ['name' => 'must-not-write'])->assertServiceUnavailable();
        $this->postJson('/api/v1/browser-operations/device-token', ['device_code' => 'restored-approved'])->assertServiceUnavailable();
        $this->assertDatabaseCount('tasks', 0);
        $this->assertSame(1, $admin->tokens()->count());
        $this->state('validating');
        $this->postJson('/api/v1/auth/login', ['username' => $admin->username, 'password' => 'secret-test-password'])->assertServiceUnavailable();
    }
}
