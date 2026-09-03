package enrollment

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteExclusiveHonorsRequestedModeUnderRestrictiveUmask(t *testing.T) {
	const helperEnvironment = "GEOFLOW_TEST_RESTRICTIVE_UMASK"
	const pathEnvironment = "GEOFLOW_TEST_WRITE_PATH"

	if os.Getenv(helperEnvironment) == "1" {
		syscall.Umask(0o077)
		if err := writeExclusive(os.Getenv(pathEnvironment), []byte("token\n"), 0o640); err != nil {
			t.Fatalf("writeExclusive() error = %v", err)
		}
		return
	}

	path := filepath.Join(t.TempDir(), "control.token")
	command := exec.Command(os.Args[0], "-test.run=^TestWriteExclusiveHonorsRequestedModeUnderRestrictiveUmask$")
	command.Env = append(os.Environ(), helperEnvironment+"=1", pathEnvironment+"="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("restrictive-umask helper failed: %v\n%s", err, output)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("written file mode = %o, want 640", info.Mode().Perm())
	}
}
