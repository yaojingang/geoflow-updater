package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/agent"
	"github.com/yaojingang/geoflow-updater/internal/authorization"
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
	stateDir           = "/var/lib/geoflow-updater"
	runtimeSocket      = "/run/geoflow-updater/geoflow-updater.sock"
	defaultMetadataURL = "https://yaojingang.github.io/geoflow-updater/metadata"
	defaultTargetsURL  = "https://yaojingang.github.io/geoflow-updater/targets"
	controlGroup       = "geoflow-updater"
)

var version = "development"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	metadataURL, targetsURL, err := releaseRepositoryURLs()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
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
	mutationAuthorization := authorization.Service{StateDir: stateDir}
	server := agent.Server{StateDir: stateDir, Version: version, Status: diagnostics, Operations: operations, Authorization: mutationAuthorization}
	application := cli.App{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Version: version,
		Enroller: enrollment.Service{
			StateDir:       stateDir,
			Releases:       releases,
			ControlGroupID: updaterGroupID,
		},
		Doctor:        diagnostics,
		Operations:    operations,
		Authorization: mutationAuthorization,
		Serve: func(ctx context.Context) error {
			if err := operations.Reconcile("primary"); err != nil && !errors.Is(err, operation.ErrActive) {
				return fmt.Errorf("reconcile interrupted operation: %w", err)
			}
			monitorDone := make(chan struct{})
			go func() {
				defer close(monitorDone)
				monitorInterruptedOperations(ctx, operations, os.Stderr)
			}()
			err := agent.ListenAndServe(ctx, runtimeSocket, server.Handler())
			<-monitorDone

			return err
		},
	}

	exitCode := application.Run(ctx, os.Args[1:])
	if ctx.Err() != nil {
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		if err := operations.Wait(recoveryCtx); err != nil {
			fmt.Fprintf(os.Stderr, "wait for updater safety recovery: %v\n", err)
			exitCode = 1
		}
		cancel()
	}
	os.Exit(exitCode)
}

func releaseRepositoryURLs() (string, string, error) {
	metadataURL := os.Getenv("GEOFLOW_UPDATER_TUF_METADATA_URL")
	targetsURL := os.Getenv("GEOFLOW_UPDATER_TUF_TARGETS_URL")
	if metadataURL == "" && targetsURL == "" {
		return defaultMetadataURL, defaultTargetsURL, nil
	}
	if metadataURL == "" || targetsURL == "" || os.Getenv("GEOFLOW_UPDATER_ALLOW_CANDIDATE_REPOSITORY") != "1" {
		return "", "", errors.New("candidate TUF repository requires both URLs and explicit root-only opt-in")
	}
	metadata, metadataErr := url.Parse(metadataURL)
	targets, targetsErr := url.Parse(targetsURL)
	if metadataErr != nil || targetsErr != nil || metadata.Scheme != "https" || targets.Scheme != "https" ||
		metadata.Host == "" || metadata.Host != targets.Host || metadata.User != nil || targets.User != nil ||
		metadata.RawQuery != "" || targets.RawQuery != "" || metadata.Fragment != "" || targets.Fragment != "" {
		return "", "", errors.New("candidate TUF repository URLs must use one HTTPS origin without credentials, query, or fragment")
	}

	return metadataURL, targetsURL, nil
}

func monitorInterruptedOperations(ctx context.Context, operations *operation.Manager, stderr io.Writer) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := operations.Reconcile("primary"); err != nil && !errors.Is(err, operation.ErrActive) {
				fmt.Fprintf(stderr, "reconcile interrupted operation: %v\n", err)
			}
		}
	}
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
