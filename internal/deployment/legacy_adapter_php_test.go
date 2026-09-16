package deployment

import (
	"os/exec"
	"testing"
)

func TestPinnedLegacyRecoveryPHPAdapter(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("PHP is required for the embedded recovery adapter integration test")
	}
	output, err := exec.Command(php, "legacy-adapter-test.php").CombinedOutput()
	if err != nil {
		t.Fatalf("fixed legacy recovery adapter: %v\n%s", err, output)
	}
	t.Log(string(output))
}
