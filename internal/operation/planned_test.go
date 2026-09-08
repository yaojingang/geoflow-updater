package operation

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/update"
)

type blockingPreview struct{ *fakeDeployment }

func (deployment *blockingPreview) Preview(ctx context.Context, _ string) (update.PlanSummary, error) {
	<-ctx.Done()
	return update.PlanSummary{}, ctx.Err()
}

func TestPreviewDeadlineReleasesInstanceLock(t *testing.T) {
	manager := &Manager{StateDir: t.TempDir(), Deployment: &blockingPreview{&fakeDeployment{}}, PreviewTimeout: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	if _, err := manager.Preview(ctx, "primary"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("preview error = %v, want deadline exceeded", err)
	}
	if time.Since(started) >= time.Second {
		t.Fatal("preview failed to apply its own deadline")
	}
	lock, err := manager.acquireLock("primary")
	if err != nil {
		t.Fatalf("preview left the instance locked: %v", err)
	}
	defer lock.Close()
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
}
