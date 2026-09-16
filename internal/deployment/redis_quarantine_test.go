package deployment

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the exact embedded fixed Lua against an isolated local Redis socket.
func TestRedisQuarantinePreservesEveryQueueStateAndIsIdempotent(t *testing.T) {
	server, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is not installed")
	}
	client, err := exec.LookPath("redis-cli")
	if err != nil {
		t.Skip("redis-cli is not installed")
	}
	dir, err := os.MkdirTemp("/private/tmp", "geoflow-redis-test-")
	if err != nil {
		dir, err = os.MkdirTemp("", "geoflow-redis-test-")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "redis.sock")
	var log bytes.Buffer
	process := exec.Command(server, "--port", "0", "--save", "", "--appendonly", "no", "--unixsocket", socket, "--unixsocketperm", "700")
	process.Stdout = &log
	process.Stderr = &log
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	ready := false
	for attempt := 0; attempt < 100; attempt++ {
		if out, err := exec.Command(client, "-s", socket, "PING").CombinedOutput(); err == nil && strings.TrimSpace(string(out)) == "PONG" {
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("isolated Redis did not become ready")
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(client, append([]string{"-s", socket, "--raw"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("Redis command failed: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("RPUSH", "app:queues:default", "ready-job")
	run("ZADD", "app:queues:default:delayed", "1", "delayed-job")
	run("ZADD", "app:queues:unknown:reserved", "1", "reserved-job")
	run("LPUSH", "app:queues:default:notify", "wake")
	run("SET", "app:queues:custom-state", "custom-job")
	run("PEXPIRE", "app:queues:custom-state", "3600000")
	run("SET", "session:unrelated", "session-value")
	source := string(recoveryRedisScript)
	extract := func(name string) string {
		t.Helper()
		start := strings.Index(source, "$"+name+" = <<<'LUA'\n")
		if start < 0 {
			t.Fatal("fixed Redis script missing")
		}
		tail := source[start+len("$"+name+" = <<<'LUA'\n"):]
		end := strings.Index(tail, "\nLUA;")
		if end < 0 {
			t.Fatal("fixed Redis terminator missing")
		}
		return tail[:end]
	}
	scan, verify := extract("lua"), extract("verify")
	epoch := strings.Repeat("a", 32)
	execute := func() {
		t.Helper()
		cursor := "0"
		for attempt := 0; attempt < 100; attempt++ {
			cursor = run("EVAL", scan, "0", epoch, cursor)
			if cursor == "0" {
				return
			}
			if strings.HasPrefix(cursor, "ERR") {
				t.Fatal(cursor)
			}
		}
		t.Fatal("queue scan did not finish")
	}
	execute()
	first := run("EVAL", verify, "0", epoch)
	if len(strings.Split(first, "\n")) != 5 {
		t.Fatalf("missing quarantined queue populations: %s", first)
	}
	execute()
	second := run("EVAL", verify, "0", epoch)
	if first != second {
		t.Fatal("repeated quarantine changed manifest")
	}
	if got := run("GET", "session:unrelated"); got != "session-value" {
		t.Fatalf("unrelated Redis data changed: %s", got)
	}
	for _, key := range []string{"app:queues:default", "app:queues:default:delayed", "app:queues:unknown:reserved", "app:queues:default:notify", "app:queues:custom-state"} {
		if got := run("EXISTS", key); got != "0" {
			t.Fatalf("live queue remains visible: %s", key)
		}
		if got := run("EXISTS", "geoflow-recovery:"+epoch+":payload:"+key); got != "1" {
			t.Fatalf("queue payload lost: %s", key)
		}
	}
	if got := run("PTTL", "geoflow-recovery:"+epoch+":payload:app:queues:custom-state"); got != "-1" {
		t.Fatalf("quarantine payload may expire: %s", got)
	}
	run("SET", "geoflow-recovery:"+epoch+":payload:app:queues:custom-state", "tampered")
	if got := run("EVAL", verify, "0", epoch); !strings.Contains(got, "quarantine changed") {
		t.Fatalf("modified quarantine accepted: %s", got)
	}
	// Redis scripts keep earlier writes if a later command fails. Record the
	// evidence before renaming so a failed manifest write cannot hide a job.
	epoch = strings.Repeat("b", 32)
	run("SET", "geoflow-recovery:"+epoch+":manifest", "wrong-type")
	run("LPUSH", "app:queues:new-unknown", "preserved-job")
	if got := run("EVAL", scan, "0", epoch, "0"); !strings.Contains(got, "WRONGTYPE") {
		t.Fatalf("expected manifest failure: %s", got)
	}
	if got := run("EXISTS", "app:queues:new-unknown"); got != "1" {
		t.Fatal("manifest failure hid the unrecorded job")
	}
}
