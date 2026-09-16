<?php
// Fixed host entry point. Only the epoch is supplied by the caller.
declare(strict_types=1);
try {
    $epoch = $argv[1] ?? '';
    if (preg_match('/^[a-f0-9]{32}$/D', $epoch) !== 1) { throw new RuntimeException('invalid_epoch'); }
    $state = json_decode(file_get_contents('/run/geoflow-recovery-control/state.json'), true, 16, JSON_THROW_ON_ERROR);
    if (($state['phase'] ?? null) !== 'validating' || ($state['epoch'] ?? null) !== $epoch) { throw new RuntimeException('invalid_boundary'); }
    require '/var/www/html/vendor/autoload.php';
    $app = require '/var/www/html/bootstrap/app.php';
    $app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();
    $lua = <<<'LUA'
local epoch, cursor = ARGV[1], ARGV[2]
local base = 'geoflow-recovery:' .. epoch .. ':'
local index = base .. 'manifest'
local scan = redis.call('SCAN', cursor, 'MATCH', '*queues:*', 'COUNT', 200)
for _,key in ipairs(scan[2]) do
    if string.sub(key, 1, 17) ~= 'geoflow-recovery:' then
        local destination = base .. 'payload:' .. key
        if redis.call('EXISTS', destination) == 1 then return redis.error_reply('quarantine collision') end
        local value = redis.call('DUMP', key)
        if value then
            local kind = redis.call('TYPE', key)['ok']
            local ttl = redis.call('PTTL', key)
            local digest = redis.sha1hex(value)
            redis.call('HSET', index, key, cjson.encode({kind,ttl,digest}))
            redis.call('RENAME', key, destination)
            redis.call('PERSIST', destination)
        end
    end
end
return scan[1]
LUA;
    $verify = <<<'LUA'
local base = 'geoflow-recovery:' .. ARGV[1] .. ':'
local entries = redis.call('HGETALL', base .. 'manifest')
local result = {}
for i=1,#entries,2 do
    local metadata = cjson.decode(entries[i+1])
    local dump = redis.call('DUMP',base .. 'payload:' .. entries[i])
    if not dump or redis.sha1hex(dump) ~= metadata[3] then return redis.error_reply('quarantine changed') end
    table.insert(result, cjson.encode({redis.sha1hex(entries[i]),metadata[1],metadata[2],metadata[3]}))
end
return result
LUA;
    $entries = [];
    foreach (config('database.redis') as $name => $connection) {
        if (in_array($name, ['client', 'options'], true)) { continue; }
        if (!is_array($connection) || ($connection['host'] ?? null) !== 'redis' || !empty($connection['url']) || (int)($connection['port'] ?? 0) !== 6379) { throw new RuntimeException('unsupported_redis_topology'); }
        $redis = Illuminate\Support\Facades\Redis::connection($name);
        $cursor = '0'; $iterations = 0;
        do {
            $cursor = (string)$redis->eval($lua, 0, $epoch, $cursor);
            if (++$iterations > 10000) { throw new RuntimeException('redis_scan_limit'); }
        } while ($cursor !== '0');
        foreach ($redis->eval($verify, 0, $epoch) as $record) {
            $entries[] = [$name, json_decode($record, true, 8, JSON_THROW_ON_ERROR)];
        }
    }
    sort($entries);
    echo json_encode(['schema_version'=>1,'status'=>'pass','epoch'=>$epoch,'entries'=>$entries], JSON_THROW_ON_ERROR)."\n";
} catch (Throwable $error) {
    fwrite(STDERR,"recovery_redis_quarantine_failed\n");
    exit(1);
}
