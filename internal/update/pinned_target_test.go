package update_test

import (
	"context"
	"github.com/yaojingang/geoflow-updater/internal/managed"
	"github.com/yaojingang/geoflow-updater/internal/update"
	"strings"
	"testing"
)

func TestPinnedAdmissionTargetDoesNotResolveANewerChannel(t *testing.T) {
	target := managed.Release{Sequence: 19, Version: "3.1.0", AppImage: "ghcr.io/yaojingang/geoflow-app@sha256:" + strings.Repeat("a", 64), WebImage: "ghcr.io/yaojingang/geoflow-web@sha256:" + strings.Repeat("b", 64), PostgresImages: map[string]string{"16": "pgvector/pgvector@sha256:" + strings.Repeat("c", 64), "18": "pgvector/pgvector@sha256:" + strings.Repeat("d", 64)}, RedisImages: map[string]string{"7": "redis@sha256:" + strings.Repeat("e", 64), "8": "redis@sha256:" + strings.Repeat("f", 64)}, ComposeTemplate: []byte("services: {}")}
	deployment := &fakeDeployment{}
	result := (update.Engine{Deployment: deployment}).RunWithOptions(context.Background(), "primary", update.Options{AllowMaintenance: true, PinnedTarget: &target}, nil)
	if result.Status != update.StatusSucceeded || result.Target.Sequence != target.Sequence || result.Target.AppImage != target.AppImage {
		t.Fatal(result)
	}
	for _, call := range deployment.calls {
		if call == "resolve" {
			t.Fatal("accepted target was resolved again")
		}
	}
}
