package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/bootstrap"
	"github.com/yaojingang/geoflow-updater/internal/tufrepo"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		initialize()
	case "publish":
		publish()
	case "refresh":
		refresh()
	case "refresh-online":
		refreshOnline()
	case "sign-bootstrap":
		signBootstrap()
	case "verify-bootstrap":
		verifyBootstrap()
	default:
		usage()
		os.Exit(2)
	}
}

func verifyBootstrap() {
	flags := flag.NewFlagSet("verify-bootstrap", flag.ExitOnError)
	manifestPath := flags.String("manifest", "", "signed bootstrap envelope JSON")
	trustedRootPath := flags.String("trusted-root", "", "trusted root metadata JSON")
	updaterVersion := flags.String("updater-version", "", "expected updater version")
	if err := flags.Parse(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	contents, err := os.ReadFile(*manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read bootstrap manifest: %v\n", err)
		os.Exit(1)
	}
	trustedRoot, err := os.ReadFile(*trustedRootPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read trusted root: %v\n", err)
		os.Exit(1)
	}
	var envelope bootstrap.Envelope
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		fmt.Fprintf(os.Stderr, "decode bootstrap manifest: %v\n", err)
		os.Exit(1)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "decode bootstrap manifest: trailing JSON value")
		os.Exit(1)
	}
	if envelope.Signed.UpdaterVersion != *updaterVersion {
		fmt.Fprintf(os.Stderr, "bootstrap updater version %q does not match %q\n", envelope.Signed.UpdaterVersion, *updaterVersion)
		os.Exit(1)
	}
	if err := bootstrap.Verify(envelope, trustedRoot, time.Now().UTC()); err != nil {
		fmt.Fprintf(os.Stderr, "verify bootstrap manifest: %v\n", err)
		os.Exit(1)
	}
}

func signBootstrap() {
	flags := flag.NewFlagSet("sign-bootstrap", flag.ExitOnError)
	manifestPath := flags.String("manifest", "", "unsigned bootstrap manifest JSON")
	targetsKeyPath := flags.String("targets-key", "", "targets role private key")
	outputPath := flags.String("output", "", "signed bootstrap envelope output")
	if err := flags.Parse(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	contents, err := os.ReadFile(*manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read bootstrap manifest: %v\n", err)
		os.Exit(1)
	}
	var manifest bootstrap.Manifest
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		fmt.Fprintf(os.Stderr, "decode bootstrap manifest: %v\n", err)
		os.Exit(1)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "decode bootstrap manifest: trailing JSON value")
		os.Exit(1)
	}
	privateKey, err := tufrepo.LoadPrivateKey(*targetsKeyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load targets key: %v\n", err)
		os.Exit(1)
	}
	envelope, err := bootstrap.Sign(manifest, privateKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sign bootstrap manifest: %v\n", err)
		os.Exit(1)
	}
	encoded, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode bootstrap envelope: %v\n", err)
		os.Exit(1)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(*outputPath, encoded, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write bootstrap envelope: %v\n", err)
		os.Exit(1)
	}
}

func initialize() {
	flags := flag.NewFlagSet("init", flag.ExitOnError)
	keysDir := flags.String("keys-dir", "", "private key output directory")
	repositoryDir := flags.String("repository-dir", "", "public TUF repository output directory")
	targetsDir := flags.String("targets-dir", "", "source target directory")
	if err := flags.Parse(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := tufrepo.Initialize(tufrepo.InitializeOptions{
		KeysDir:       *keysDir,
		RepositoryDir: *repositoryDir,
		TargetsDir:    *targetsDir,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "initialize TUF repository: %v\n", err)
		os.Exit(1)
	}
}

func publish() {
	flags := flag.NewFlagSet("publish", flag.ExitOnError)
	repositoryDir := flags.String("repository-dir", "", "public TUF repository directory")
	targetsDir := flags.String("targets-dir", "", "source target directory")
	targetsKey := flags.String("targets-key", "", "targets role private key")
	snapshotKey := flags.String("snapshot-key", "", "snapshot role private key")
	timestampKey := flags.String("timestamp-key", "", "timestamp role private key")
	if err := flags.Parse(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := tufrepo.Publish(tufrepo.PublishOptions{
		RepositoryDir:    *repositoryDir,
		TargetsDir:       *targetsDir,
		TargetsKeyPath:   *targetsKey,
		SnapshotKeyPath:  *snapshotKey,
		TimestampKeyPath: *timestampKey,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "publish TUF repository: %v\n", err)
		os.Exit(1)
	}
}

func refresh() {
	flags := flag.NewFlagSet("refresh", flag.ExitOnError)
	repositoryDir := flags.String("repository-dir", "", "public TUF repository directory")
	targetsKey := flags.String("targets-key", "", "targets role private key")
	snapshotKey := flags.String("snapshot-key", "", "snapshot role private key")
	timestampKey := flags.String("timestamp-key", "", "timestamp role private key")
	if err := flags.Parse(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := tufrepo.Refresh(tufrepo.RefreshOptions{
		RepositoryDir:    *repositoryDir,
		TargetsKeyPath:   *targetsKey,
		SnapshotKeyPath:  *snapshotKey,
		TimestampKeyPath: *timestampKey,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "refresh TUF repository: %v\n", err)
		os.Exit(1)
	}
}

func refreshOnline() {
	flags := flag.NewFlagSet("refresh-online", flag.ExitOnError)
	repositoryDir := flags.String("repository-dir", "", "public TUF repository directory")
	snapshotKey := flags.String("snapshot-key", "", "snapshot role private key")
	timestampKey := flags.String("timestamp-key", "", "timestamp role private key")
	if err := flags.Parse(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := tufrepo.RefreshOnline(tufrepo.RefreshOnlineOptions{
		RepositoryDir:    *repositoryDir,
		SnapshotKeyPath:  *snapshotKey,
		TimestampKeyPath: *timestampKey,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "refresh online TUF metadata: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  geoflow-tuf init --keys-dir PATH --repository-dir PATH --targets-dir PATH")
	fmt.Fprintln(os.Stderr, "  geoflow-tuf publish --repository-dir PATH --targets-dir PATH --targets-key PATH --snapshot-key PATH --timestamp-key PATH")
	fmt.Fprintln(os.Stderr, "  geoflow-tuf refresh --repository-dir PATH --targets-key PATH --snapshot-key PATH --timestamp-key PATH")
	fmt.Fprintln(os.Stderr, "  geoflow-tuf refresh-online --repository-dir PATH --snapshot-key PATH --timestamp-key PATH")
	fmt.Fprintln(os.Stderr, "  geoflow-tuf sign-bootstrap --manifest PATH --targets-key PATH --output PATH")
	fmt.Fprintln(os.Stderr, "  geoflow-tuf verify-bootstrap --manifest PATH --trusted-root PATH --updater-version VERSION")
}
