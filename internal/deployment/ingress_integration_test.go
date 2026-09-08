package deployment

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
)

// Uses only uniquely named disposable containers; it never touches enrolled sites.
func TestDockerIngressSwitchKeepsInflightStream(t *testing.T) {
	if os.Getenv("GEOFLOW_DOCKER_TEST") != "1" {
		t.Skip("set GEOFLOW_DOCKER_TEST=1 to exercise real Docker/Nginx")
	}
	prefix := fmt.Sprintf("geoflow-bg-test-%d", time.Now().UnixNano())
	directory := canonicalTemp(t)
	docker := func(args ...string) string {
		t.Helper()
		out, e := exec.Command("docker", args...).CombinedOutput()
		if e != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), e, out)
		}
		return strings.TrimSpace(string(out))
	}
	docker("network", "create", "--subnet", fmt.Sprintf("198.18.%d.0/28", time.Now().UnixNano()%250), prefix)
	t.Cleanup(func() {
		for _, name := range []string{"edge", "blue", "green"} {
			_ = exec.Command("docker", "rm", "-f", prefix+"-"+name).Run()
		}
		_ = exec.Command("docker", "network", "rm", prefix).Run()
	})
	program := `import http.server,os,time
class Handler(http.server.BaseHTTPRequestHandler):
 def do_GET(self):
  slot=os.environ['SLOT']; self.send_response(200); self.send_header('Content-Type','text/event-stream'); self.end_headers()
  self.wfile.write((slot+'-start\n').encode());self.wfile.flush()
  if self.path=='/slow':time.sleep(3)
  self.wfile.write((slot+'-end\n').encode());self.wfile.flush()
 def log_message(self,*args):pass
http.server.ThreadingHTTPServer(('0.0.0.0',80),Handler).serve_forever()
`
	for _, slot := range []string{"blue", "green"} {
		docker("run", "-d", "--name", prefix+"-"+slot, "--network", prefix, "--network-alias", slot+"-web", "-e", "SLOT="+slot, "python:3.12-alpine", "python", "-u", "-c", program)
	}
	blue, e := renderIngress("blue", 7, "http", 80)
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, filepath.Join(directory, "nginx.conf"), blue)
	if e := os.Chmod(filepath.Join(directory, "nginx.conf"), 0644); e != nil {
		t.Fatal(e)
	}
	assets := filepath.Join(directory, "assets")
	writeTest(t, filepath.Join(assets, "old-123.js"), []byte("retained asset"))
	_ = os.Chmod(filepath.Join(assets, "old-123.js"), 0644)
	docker("run", "-d", "--name", prefix+"-edge", "--network", prefix, "-p", "127.0.0.1::80", "-v", directory+":/etc/geoflow:ro", "-v", assets+":/srv/geoflow-assets:ro", "nginx:1.31.1-alpine", "nginx", "-c", "/etc/geoflow/nginx.conf", "-g", "daemon off;")
	address := docker("port", prefix+"-edge", "80/tcp")
	base := "http://" + address
	client := &http.Client{Timeout: 10 * time.Second}
	ready := false
	for i := 0; i < 50; i++ {
		response, e := client.Get(base + "/")
		if e == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("test backend did not become ready")
	}
	response, e := client.Get(base + "/slow")
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	line, e := reader.ReadString('\n')
	if e != nil || line != "blue-start\n" {
		t.Fatalf("stream not started: %q %v", line, e)
	}
	var wg sync.WaitGroup
	failures := make(chan string, 200)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			r, e := client.Get(base + "/")
			if e != nil {
				failures <- e.Error()
				continue
			}
			body, e := io.ReadAll(r.Body)
			r.Body.Close()
			if r.StatusCode != 200 || e != nil || !(strings.Contains(string(body), "blue-end") || strings.Contains(string(body), "green-end")) {
				failures <- fmt.Sprintf("status %d body %s err %v", r.StatusCode, body, e)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	green, e := renderIngress("green", 8, "http", 80)
	if e != nil {
		t.Fatal(e)
	}
	if e := replaceContents(filepath.Join(directory, "candidate.conf"), green, 0644); e != nil {
		t.Fatal(e)
	}
	docker("exec", prefix+"-edge", "nginx", "-t", "-c", "/etc/geoflow/candidate.conf")
	if e := replaceContents(filepath.Join(directory, "nginx.conf"), green, 0644); e != nil {
		t.Fatal(e)
	}
	docker("exec", prefix+"-edge", "nginx", "-s", "reload", "-c", "/etc/geoflow/nginx.conf")
	rest, e := io.ReadAll(reader)
	if e != nil || string(rest) != "blue-end\n" {
		t.Fatalf("inflight stream was interrupted: %q %v", rest, e)
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	r, e := client.Get(base + "/")
	if e != nil {
		t.Fatal(e)
	}
	body, e := io.ReadAll(r.Body)
	r.Body.Close()
	if e != nil || !strings.Contains(string(body), "green-end") || r.Header.Get("X-GEOFlow-Release") != "8" {
		t.Fatalf("new traffic not switched: %s %v", body, e)
	}
	assetRequest, e := http.NewRequest(http.MethodGet, base+"/build/assets/old-123.js", nil)
	if e != nil {
		t.Fatal(e)
	}
	assetRequest.Header.Set("Origin", "null")
	r, e = client.Do(assetRequest)
	if e != nil {
		t.Fatal(e)
	}
	body, e = io.ReadAll(r.Body)
	r.Body.Close()
	if e != nil || r.StatusCode != http.StatusOK || string(body) != "retained asset" {
		t.Fatalf("old assets unavailable: %s %v", body, e)
	}
	if origin := r.Header.Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Fatalf("retained asset CORS header = %q; sandbox theme previews with Origin:null require *", origin)
	}
	t.Log("100 continuous requests had no errors; old stream completed; new requests served green; retained asset remained available to sandbox theme previews")
}

// Parse the actual generated topology with Compose without creating any containers.
func TestDockerIngressTopologyConfiguration(t *testing.T) {
	if os.Getenv("GEOFLOW_DOCKER_TEST") != "1" {
		t.Skip("set GEOFLOW_DOCKER_TEST=1 to validate generated Compose")
	}
	root := canonicalTemp(t)
	directory := filepath.Join(root, "managed")
	release := testRelease(t, true)
	config := instance.Config{ID: "primary", Root: root, ReleaseSequence: release.Sequence}
	environment := fmt.Sprintf("GEOFLOW_INSTANCE_ROOT=%s\nGEOFLOW_INSTANCE_ID=primary\nGEOFLOW_APP_IMAGE=%s\nGEOFLOW_WEB_IMAGE=%s\nGEOFLOW_POSTGRES_IMAGE=%s\nGEOFLOW_REDIS_IMAGE=%s\nGEOFLOW_POSTGRES_DATA_DIR=%s\nGEOFLOW_POSTGRES_CONTAINER_DATA_DIR=/var/lib/postgresql/data\nGEOFLOW_UPDATER_GROUP_ID=987\nDB_PASSWORD=ephemeral-config-test\n", root, release.AppImage, release.WebImage, release.PostgresImages["16"], release.RedisImages["7"], filepath.Join(root, "postgres"))
	envPath := filepath.Join(root, ".env.prod")
	writeTest(t, envPath, []byte(environment))
	for _, slot := range []string{"blue", "green"} {
		app, infra, err := renderTopology(release.ComposeTemplate, config, slot, directory)
		if err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string][]byte{slot: app, "infra": infra} {
			path := filepath.Join(directory, name+".yml")
			writeTest(t, path, data)
			arguments := append(composeArguments(root, envPath, path), "config", "--quiet")
			if output, err := exec.Command("docker", arguments...).CombinedOutput(); err != nil {
				t.Fatalf("generated %s Compose configuration: %v\n%s", name, err, output)
			}
		}
	}
}
