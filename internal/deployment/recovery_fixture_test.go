package deployment

import (
	"context"
	"io"
	"strings"
)

const recoveryInspectFixture = `{"schema_version":1,"status":"pass","admin_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","theme_revisions":0}`

func isRecoveryInspect(args []string) bool {
	cmd := strings.Join(args, " ")
	return strings.Contains(cmd, "geoflow:recovery") && strings.Contains(cmd, "--phase=inspect")
}

type recoveryFixtureRunner functionRunner

func (f recoveryFixtureRunner) Run(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
	// Existing topology expectations focus on service ordering around the restore.
	// Dedicated runtime tests verify overlay contents and use unfiltered commands.
	visible := []string{}
	for index := 0; index < len(args); index++ {
		if args[index] == "-f" && index+1 < len(args) && strings.HasSuffix(args[index+1], "/recovery-runtime.yml") {
			index++
			continue
		}
		visible = append(visible, args[index])
	}
	if err := functionRunner(f).Run(ctx, in, out, name, visible...); err != nil {
		return err
	}
	if isRecoveryInspect(args) {
		_, err := io.WriteString(out, recoveryInspectFixture)
		return err
	}
	return nil
}
