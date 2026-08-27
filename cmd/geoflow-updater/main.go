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
	"github.com/yaojingang/geoflow-updater/internal/deployment"
	"github.com/yaojingang/geoflow-updater/internal/doctor"
	"github.com/yaojingang/geoflow-updater/internal/enrollment"
	"github.com/yaojingang/geoflow-updater/internal/operation"
	"github.com/yaojingang/geoflow-updater/internal/recovery"
	"github.com/yaojingang/geoflow-updater/internal/tufclient"
	"github.com/yaojingang/geoflow-updater/internal/update"
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
	deployments := &deployment.Service{
		StateDir:   stateDir,
		Releases:   releases,
		Doctor:     diagnostics,
		Runner:     deployment.RealRunner{},
		Recoveries: recovery.Store{BackupRoot: "/var/backups/geoflow-updater", Keep: 5},
	}
	operations := &operation.Manager{
		StateDir:   stateDir,
		Context:    ctx,
		Deployment: deployments,
		Engine:     update.Engine{Deployment: deployments},
	}
	server := agent.Server{StateDir: stateDir, Version: version, Status: diagnostics, Operations: operations}
	application := cli.App{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Version: version,
		Enroller: enrollment.Service{
			StateDir:       stateDir,
			Releases:       releases,
			ControlGroupID: updaterGroupID,
		},
		Doctor:     diagnostics,
		Operations: operations,
		Serve: func(ctx context.Context) error {
			if err := operations.Reconcile("primary"); err != nil {
				return fmt.Errorf("reconcile interrupted operation: %w", err)
			}
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
