package recoverycontrol_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/recoverycontrol"
)

func TestRestoreTransactionRetainsEpochAcrossRestartAndAdvancesForNewRestore(t *testing.T) {
	s := recoverycontrol.Store{StateDir: t.TempDir()}
	first, err := s.Begin("primary", "20260827T123456Z-1234abcd", "recovery-transaction-1", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if first.State.Phase != "restoring" || len(first.State.Epoch) != 32 || first.State.MinimumUpdaterProtocol != 5 {
		t.Fatalf("state=%+v", first.State)
	}
	restarted := recoverycontrol.Store{StateDir: s.StateDir}
	retry, err := restarted.Begin("primary", "20260827T123456Z-1234abcd", "recovery-transaction-1", strings.Repeat("a", 64))
	if err != nil || retry.State.Epoch != first.State.Epoch || retry.State.HostID != first.State.HostID {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	if _, err := restarted.Begin("primary", "20260827T123456Z-5678abcd", "recovery-transaction-1", strings.Repeat("a", 64)); err == nil {
		t.Fatal("reused transaction with different recovery point")
	}
	next, err := restarted.Begin("primary", "20260827T123456Z-1234abcd", "recovery-transaction-2", strings.Repeat("a", 64))
	if err != nil || next.State.Epoch == first.State.Epoch || next.State.HostID != first.State.HostID {
		t.Fatalf("new restore=%+v err=%v", next, err)
	}
	data, err := os.ReadFile(filepath.Join(s.StateDir, "recovery-control", "primary", "public", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var public map[string]any
	if err := json.Unmarshal(data, &public); err != nil {
		t.Fatal(err)
	}
	if len(public) != 7 || public["phase"] != "restoring" || public["point_id"] != nil || public["admin_digest"] != nil {
		t.Fatalf("public state exposes authority: %s", data)
	}
}

func TestLostAuthorityAndHigherProtocolRemainClosed(t *testing.T) {
	for _, failure := range []string{"missing-authority", "higher-protocol", "missing-proof"} {
		t.Run(failure, func(t *testing.T) {
			s := recoverycontrol.Store{StateDir: t.TempDir()}
			if _, err := s.Begin("primary", "20260827T123456Z-1234abcd", "recovery-transaction", strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "missing-authority":
				if err := os.Remove(filepath.Join(s.StateDir, "recovery-control", "primary", "authority.json")); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Initialize("primary"); err == nil {
					t.Fatal("lost authority silently initialized a ready epoch")
				}
			case "higher-protocol":
				if err := os.WriteFile(filepath.Join(s.StateDir, "minimum-updater-protocol"), []byte("6\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Initialize("primary"); err == nil {
					t.Fatal("higher protocol floor was overwritten")
				}
			case "missing-proof":
				if _, err := s.Restored("primary", "recovery-transaction"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.OpenHTTP("primary", "recovery-transaction"); err == nil {
					t.Fatal("HTTP opened without Core and Redis evidence")
				}
			}
		})
	}
}

func TestCompletedRecoveryKeepsProtocolFloorWithoutAnActiveOperation(t *testing.T) {
	s := recoverycontrol.Store{StateDir: t.TempDir()}
	if _, err := s.Initialize("primary"); err != nil {
		t.Fatal(err)
	}
	if err := recoverycontrol.CheckProtocol(s.StateDir, 4); err == nil {
		t.Fatal("protocol 4 accepted a managed recovery host")
	}
	if err := recoverycontrol.CheckProtocol(s.StateDir, 5); err != nil {
		t.Fatal(err)
	}
}

func TestPublicDirectoryRemainsReadableWithDaemonUmask(t *testing.T) {
	previous := syscall.Umask(0027)
	defer syscall.Umask(previous)
	s := recoverycontrol.Store{StateDir: t.TempDir()}
	if _, err := s.Initialize("primary"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.PublicDir("primary"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("PHP cannot traverse directory bind mount: mode=%o", info.Mode().Perm())
	}
}

func TestCheckpointFreezesAdministratorProofWithoutRotatingEpoch(t *testing.T) {
	s := recoverycontrol.Store{StateDir: t.TempDir()}
	before, err := s.Initialize("primary")
	if err != nil {
		t.Fatal(err)
	}
	point, tx, digest := "20260827T123456Z-1234abcd", "checkpoint-transaction", strings.Repeat("a", 64)
	if err := s.CaptureCheckpoint("primary", point, tx, digest); err != nil {
		t.Fatal(err)
	}
	restarted := recoverycontrol.Store{StateDir: s.StateDir}
	checkpoint, err := restarted.Checkpoint("primary", point, tx)
	if err != nil || checkpoint.AdminDigest != digest {
		t.Fatalf("checkpoint=%+v err=%v", checkpoint, err)
	}
	after, err := restarted.Read("primary")
	if err != nil || after.State.Epoch != before.State.Epoch || after.State.Phase != "ready" {
		t.Fatalf("capture changed source generation: %+v %v", after, err)
	}
	if err := restarted.CaptureCheckpoint("primary", point, tx, strings.Repeat("b", 64)); err == nil {
		t.Fatal("checkpoint proof could be replaced after migration")
	}
	if _, err := restarted.Checkpoint("primary", "20260827T123456Z-5678abcd", tx); err == nil {
		t.Fatal("checkpoint accepted a different restore source")
	}
}
