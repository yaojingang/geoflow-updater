package cli_test

import (
	"bytes"
	"context"
	"github.com/yaojingang/geoflow-updater/internal/cli"
	"testing"
)

func TestInstallerCanReadUpdaterProtocolWithoutStartingOperations(t *testing.T) {
	var out bytes.Buffer
	app := cli.App{Stdout: &out}
	if code := app.Run(context.Background(), []string{"protocol"}); code != 0 || out.String() != "5\n" {
		t.Fatalf("protocol exit=%d output=%q", code, out.String())
	}
}
