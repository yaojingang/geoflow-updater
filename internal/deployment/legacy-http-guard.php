<?php

declare(strict_types=1);

/** A fixed, read-only HTTP adapter. Restored background work stays held on Core 3.1.0. */
final class GeoFlowLegacyHttpGuard
{
    public const SESSION_EPOCH = 'admin_recovery_epoch';

    public function __construct(private readonly string $control = '/run/geoflow-recovery-control/state.json') {}

    public static function allowsCommand(array $arguments): bool
    {
        $script = $arguments[0] ?? '';
        if (in_array($script, ['/run/geoflow-recovery-adapter/legacy-recovery.php', '/run/geoflow-recovery-redis.php'], true)) {
            return true;
        }
        return in_array($script, ['artisan', '/var/www/html/artisan'], true)
            && in_array($arguments[1] ?? '', ['up', 'down', 'list', 'help', 'migrate:status', 'config:cache', 'config:clear', 'route:cache', 'route:clear', 'view:cache', 'view:clear', 'optimize', 'optimize:clear', 'storage:link'], true);
    }

    public static function allowsRoute(string $method, ?string $name, string $path): bool
    {
        if (in_array($method, ['GET', 'HEAD'], true)) {
            // This list is audited against the pinned source. Unknown GET handlers can create data.
            return in_array($name, [
                'site.asset.favicon', 'site.asset', 'pwa.launch', 'site.home', 'site.about', 'site.robots',
                'site.sitemap', 'site.sitemap.shard', 'site.archive', 'site.archive.month', 'site.category',
                'site.article', 'site.lead-forms.show', 'admin.entry', 'admin.login', 'admin.dashboard',
            ], true) || preg_match('~\Aapi/v1/(?:catalog|tasks(?:/[0-9]+(?:/jobs)?)?|jobs/[0-9]+|articles(?:/[0-9]+)?)\z~', $path) === 1;
        }
        if ($method === 'POST' && in_array($name, ['admin.login.attempt', 'admin.logout'], true)) {
            return true;
        }
        return ($method === 'POST' && in_array($path, ['api/v1/auth/login', 'api/v1/auth/logout'], true))
            || ($method === 'DELETE' && $path === 'api/v1/browser-operations/session');
    }

    public function handle(Illuminate\Http\Request $request, Closure $next): Symfony\Component\HttpFoundation\Response
    {
        $state = GeoFlowLegacyRecoveryAdapter::state($this->control);
        if (!in_array($state['phase'], ['ready', 'http_ready'], true)) {
            return $this->unavailable('recovery_in_progress');
        }
        if (!self::allowsRoute($request->method(), $request->route()?->getName(), $request->path())) {
            return $this->unavailable('recovery_background_held');
        }
        $guard = null;
        if ($request->hasSession()) {
            $guard = Illuminate\Support\Facades\Auth::guard('admin');
            if ($guard->check() && $request->session()->get(self::SESSION_EPOCH) !== $state['epoch']) {
                $guard->logout();
                $request->session()->invalidate();
                $request->session()->regenerateToken();
            }
            if ($request->routeIs('admin.login.attempt')) {
                $request->merge(['remember' => false]);
            }
        }
        $render = fn () => $next($request);
        if ($request->routeIs('site.*') && in_array($request->method(), ['GET', 'HEAD'], true)) {
            // The pinned public article controller suppresses counters in its existing preview context.
            // Preserve the active theme and public root so links and rendering remain unchanged.
            $response = app(App\Support\Site\SiteThemePreviewContext::class)->run(
                App\Support\Site\SiteThemeViewResolver::activeThemeId(), $request->getSchemeAndHttpHost().$request->getBaseUrl(), $render
            );
        } else {
            $response = $render();
        }
        $current = GeoFlowLegacyRecoveryAdapter::state($this->control);
        if ($current['epoch'] !== $state['epoch'] || !in_array($current['phase'], ['ready', 'http_ready'], true)) {
            if ($guard !== null) {
                $guard->logout();
                $request->session()->invalidate();
            }
            return $this->unavailable('recovery_epoch_conflict');
        }
        if ($guard !== null && $request->routeIs('admin.login.attempt') && $guard->check()) {
            $request->session()->put(self::SESSION_EPOCH, $state['epoch']);
        }

        return $response;
    }

    private function unavailable(string $code): Symfony\Component\HttpFoundation\JsonResponse
    {
        return new Symfony\Component\HttpFoundation\JsonResponse(['success' => false, 'error' => ['code' => $code, 'message' => '恢复后的后台任务仍需核对；此版本仅开放读取和重新登录。']], 503);
    }

    public static function install(Illuminate\Contracts\Foundation\Application $app): void
    {
        $app['router']->pushMiddlewareToGroup('web', self::class);
        $app['router']->pushMiddlewareToGroup('api', self::class);
        $app['router']->aliasMiddleware('site.view_log', GeoFlowLegacyReadOnlyPass::class);
    }

    public static function run(): void
    {
        if (PHP_SAPI === 'cli') {
            if (!self::allowsCommand($GLOBALS['argv'] ?? [])) {
                fwrite(STDERR, "recovery_background_held: use the fixed host recovery workflow\n");
                exit(1);
            }
            return;
        }
        try {
            require_once __DIR__.'/legacy-recovery.php';
            $state = GeoFlowLegacyRecoveryAdapter::state();
            if (!in_array($state['phase'], ['ready', 'http_ready'], true)
                || hash_file('sha256', GeoFlowLegacyRecoveryAdapter::ROOT.'/bootstrap/app.php') !== '92fbc3017365ca1d0f0a3703e865ae9c03bd2cfbf977db7e6ac421eb65fb8a67') {
                throw new RuntimeException('recovery_in_progress');
            }
            define('LARAVEL_START', microtime(true));
            require GeoFlowLegacyRecoveryAdapter::ROOT.'/vendor/autoload.php';
            $app = require GeoFlowLegacyRecoveryAdapter::ROOT.'/bootstrap/app.php';
            $app->make(Illuminate\Contracts\Http\Kernel::class)->bootstrap();
            self::install($app);
            Illuminate\Queue\Queue::createPayloadUsing(static function (): never {
                throw new RuntimeException('recovery_background_held');
            });
            $app->handleRequest(Illuminate\Http\Request::capture());
            exit;
        } catch (Throwable) {
            http_response_code(503);
            header('Content-Type: application/json');
            echo '{"success":false,"error":{"code":"recovery_state_unavailable","message":"Host recovery validation is required."}}';
            exit;
        }
    }
}

/** Suppress the pinned public analytics writer while restored work is held. */
final class GeoFlowLegacyReadOnlyPass
{
    public function handle(Illuminate\Http\Request $request, Closure $next): Symfony\Component\HttpFoundation\Response
    {
        return $next($request);
    }
}

// Loading the class in the adapter test runner does not run the application.
if (realpath((string)ini_get('auto_prepend_file')) === __FILE__) {
    GeoFlowLegacyHttpGuard::run();
}
