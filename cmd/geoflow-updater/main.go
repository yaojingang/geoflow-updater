package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"

	"github.com/yaojingang/geoflow-updater/internal/agent"
	"github.com/yaojingang/geoflow-updater/internal/cli"
	"github.com/yaojingang/geoflow-updater/internal/doctor"
	"github.com/yaojingang/geoflow-updater/internal/enrollment"
	"github.com/yaojingang/geoflow-updater/internal/tufclient"
	trust "github.com/yaojingang/geoflow-updater/tuf"
)

const (
	stateDir      = "/var/lib/geoflow-updater"
	runtimeSocket = "/run/geoflow-updater/geoflow-updater.sock"
	metadataURL   = "https://yaojingang.github.io/geoflow-updater/metadata"
	targetsURL    = "https://yaojingang.github.io/geoflow-updater/targets"
	controlGroup  = "geoflow-updater"
)

var version = "development"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	releases := tufclient.Client{
		MetadataURL: metadataURL,
		TargetsURL:  targetsURL,
		CacheDir:    stateDir + "/tuf",
		TrustedRoot: trust.TrustedRoot,
	}
	diagnostics := doctor.Service{StateDir: stateDir}
	server := agent.Server{StateDir: stateDir, Version: version, Status: diagnostics}
	application := cli.App{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Version: version,
		Enroller: enrollment.Service{
			StateDir:       stateDir,
			Releases:       releases,
			ControlGroupID: updaterGroupID,
		},
		Doctor: diagnostics,
		Serve: func(ctx context.Context) error {
			return agent.ListenAndServe(ctx, runtimeSocket, server.Handler())
		},
	}

	os.Exit(application.Run(ctx, os.Args[1:]))
}

func updaterGroupID() (int, error) {
	group, err := user.LookupGroup(controlGroup)
	if err != nil {
		return 0, fmt.Errorf("lookup %s group: %w", controlGroup, err)
	}
	groupID, err := strconv.Atoi(group.Gid)
	if err != nil {
		return 0, fmt.Errorf("parse %s group id: %w", controlGroup, err)
	}

	return groupID, nil
}
