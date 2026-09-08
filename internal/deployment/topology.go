package deployment

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yaojingang/geoflow-updater/internal/instance"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"gopkg.in/yaml.v3"
)

const LayoutBlueGreen = "blue-green"

func validSlot(slot string) bool { return slot == "blue" || slot == "green" }
func otherSlot(slot string) string {
	if slot == "blue" {
		return "green"
	}
	return "blue"
}

func renderTopology(template []byte, config instance.Config, slot string, instanceDir string) ([]byte, []byte, error) {
	if !validSlot(slot) || !instanceIDPattern.MatchString(config.ID) {
		return nil, nil, errors.New("invalid deployment slot or instance")
	}
	var document map[string]any
	if err := yaml.Unmarshal(template, &document); err != nil {
		return nil, nil, err
	}
	services, ok := document["services"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("signed Compose has no services")
	}
	for _, required := range []string{"postgres", "redis", "init", "app", "web", "queue", "knowledge-queue", "scheduler", "reverb"} {
		if _, ok := services[required].(map[string]any); !ok {
			return nil, nil, fmt.Errorf("signed Compose is missing %s", required)
		}
	}
	prefix := "geoflow-" + config.ID
	appServices := map[string]any{}
	infraServices := map[string]any{}
	for name, value := range services {
		service, ok := value.(map[string]any)
		if !ok || !composeServicePattern.MatchString(name) {
			return nil, nil, errors.New("invalid signed Compose service")
		}
		if name == "system-update-queue" {
			continue
		}
		delete(service, "container_name")
		delete(service, "depends_on")
		delete(service, "ports")
		delete(service, "build")
		if name == "postgres" || name == "redis" {
			service["networks"] = map[string]any{"data": map[string]any{"aliases": []string{name}}}
			infraServices[name] = service
			continue
		}
		environment, ok := service["environment"].(map[string]any)
		if !ok {
			environment = map[string]any{}
		}
		if name != "web" {
			if name != "app" {
				service["user"] = "33:33"
			}
			environment["AUTO_MIGRATE"] = "false"
			environment["AUTO_INSTALL_ONCE"] = "false"
			environment["AUTO_OPTIMIZE"] = "false"
			environment["AUTO_FIX_STORAGE_PERMISSIONS"] = "false"
			environment["GEOFLOW_SECURITY_UPGRADE_DRAIN_CONFIRMED"] = "false"
			environment["GEOFLOW_SECURITY_FRESH_INSTALL_CONFIRMED"] = "false"
			environment["REVERB_SCALING_ENABLED"] = "true"
			environment["VIEW_COMPILED_PATH"] = "/var/www/html/bootstrap/cache/views"
			environment["GEOFLOW_DEPLOYMENT_SLOT"] = slot
			environment["GEOFLOW_RELEASE_SEQUENCE"] = strconv.FormatUint(config.ReleaseSequence, 10)
			volumes, _ := service["volumes"].([]any)
			service["volumes"] = append(volumes, filepath.Join(instanceDir, "slots", slot, "views")+":/var/www/html/bootstrap/cache/views")
			service["networks"] = map[string]any{"app": nil, "data": nil}
		} else {
			service["networks"] = map[string]any{"app": nil, "edge": map[string]any{"aliases": []string{slot + "-web"}}}
		}
		service["environment"] = environment
		if name == "init" {
			service["restart"] = "no"
		}
		appServices[name] = service
	}
	app := map[string]any{
		"name": prefix + "-" + slot, "services": appServices,
		"networks": map[string]any{
			"app":  map[string]any{},
			"data": map[string]any{"external": true, "name": prefix + "-data"},
			"edge": map[string]any{"external": true, "name": prefix + "-edge"},
		},
	}
	infraServices["edge"] = map[string]any{
		"image":      "${GEOFLOW_WEB_IMAGE:?signed GEOFLOW_WEB_IMAGE is required}",
		"entrypoint": []string{"nginx"}, "command": []string{"-c", "/etc/geoflow/nginx.conf", "-g", "daemon off;"},
		"ports": []string{"${WEB_PORT:-18080}:80"}, "restart": "unless-stopped",
		"volumes":     []string{filepath.Join(instanceDir, "infra", "traffic") + ":/etc/geoflow:ro", filepath.Join(instanceDir, "assets") + ":/srv/geoflow-assets:ro"},
		"networks":    map[string]any{"edge": nil},
		"healthcheck": map[string]any{"test": []string{"CMD-SHELL", "wget -q -O /dev/null http://127.0.0.1:8081/ready"}, "interval": "5s", "timeout": "3s", "retries": 12},
	}
	infra := map[string]any{"name": prefix + "-infra", "services": infraServices,
		"networks": map[string]any{"data": map[string]any{"name": prefix + "-data"}, "edge": map[string]any{"name": prefix + "-edge"}},
	}
	appBytes, err := yaml.Marshal(app)
	if err != nil {
		return nil, nil, err
	}
	infraBytes, err := yaml.Marshal(infra)
	return appBytes, infraBytes, err
}

func renderIngress(slot string, sequence uint64, scheme string, port int) ([]byte, error) {
	if !validSlot(slot) || sequence == 0 || (scheme != "http" && scheme != "https") || port < 1 || port > 65535 {
		return nil, errors.New("invalid ingress identity")
	}
	return []byte(fmt.Sprintf(`worker_processes auto;
pid /tmp/nginx.pid;
events { worker_connections 4096; }
http {
  include /etc/nginx/mime.types;
  default_type application/octet-stream;
  resolver 127.0.0.11 valid=5s ipv6=off;
  map $http_upgrade $connection_upgrade { default upgrade; '' close; }
  server {
    listen 127.0.0.1:8081;
    location = /ready { return 200 "ready\n"; }
    location = /release { default_type application/json; return 200 '{"slot":"%s","sequence":%d}'; }
  }
  server {
    listen 80 default_server;
    server_name _;
    client_max_body_size 128m;
    add_header X-GEOFlow-Release "%d" always;
    location ^~ /build/assets/ {
      alias /srv/geoflow-assets/;
      expires 1y;
      add_header Cache-Control "public, immutable";
      add_header Access-Control-Allow-Origin "*";
    }
    location / {
      set $active_web %s-web;
      proxy_pass http://$active_web:80;
      proxy_http_version 1.1;
      proxy_set_header Host $http_host;
      proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
      proxy_set_header X-Forwarded-Proto %s;
      proxy_set_header X-Forwarded-Port %d;
      proxy_set_header Upgrade $http_upgrade;
      proxy_set_header Connection $connection_upgrade;
      proxy_buffering off;
      proxy_read_timeout 86400s;
      proxy_send_timeout 86400s;
      proxy_next_upstream off;
    }
  }
}
`, slot, sequence, sequence, slot, scheme, port)), nil
}

func environmentValue(contents []byte, key string) (string, error) {
	var value string
	found := false
	for _, line := range strings.Split(string(contents), "\n") {
		name, raw, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || name != key {
			continue
		}
		if found {
			return "", fmt.Errorf("duplicate environment key %s", key)
		}
		found = true
		value = strings.Trim(strings.TrimSpace(raw), "\"'")
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("invalid environment value")
	}
	return value, nil
}

func releaseStrategy(release managed.Release) string {
	plan, err := release.Plan()
	if err == nil {
		return plan.Strategy
	}
	return managed.StrategyMaintenance
}
