package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/yaojingang/geoflow-updater/internal/doctor"
	"github.com/yaojingang/geoflow-updater/internal/enrollment"
)

type Enroller interface {
	Enroll(context.Context, enrollment.Request) (enrollment.Result, error)
}

type Diagnostician interface {
	Run(context.Context, string) doctor.Report
}

type App struct {
	Stdout   io.Writer
	Stderr   io.Writer
	Version  string
	Enroller Enroller
	Doctor   Diagnostician
	Serve    func(context.Context) error
}

func (app App) Run(ctx context.Context, arguments []string) int {
	stdout := app.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := app.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	if len(arguments) == 0 {
		app.usage(stderr)
		return 2
	}

	switch arguments[0] {
	case "enroll":
		return app.enroll(ctx, arguments[1:], stdout, stderr)
	case "doctor":
		return app.doctor(ctx, arguments[1:], stdout, stderr)
	case "serve":
		return app.serve(ctx, arguments[1:], stderr)
	case "version":
		fmt.Fprintln(stdout, app.Version)
		return 0
	case "help", "--help", "-h":
		app.usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", arguments[0])
		app.usage(stderr)
		return 2
	}
}

func (app App) enroll(ctx context.Context, arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("enroll", flag.ContinueOnError)
	flags.SetOutput(stderr)
	instanceID := flags.String("instance-id", "primary", "managed instance identifier")
	instanceRoot := flags.String("instance-root", "", "absolute GEOFlow deployment root")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*instanceRoot) == "" {
		fmt.Fprintln(stderr, "enroll requires --instance-root and accepts no positional arguments")
		return 2
	}
	if app.Enroller == nil {
		fmt.Fprintln(stderr, "enrollment service is unavailable")
		return 1
	}
	result, err := app.Enroller.Enroll(ctx, enrollment.Request{InstanceID: *instanceID, Root: *instanceRoot})
	if err != nil {
		fmt.Fprintf(stderr, "enrollment failed: %v\n", err)
		return 1
	}
	version := result.Instance.Version
	if version == "" {
		version = "trusted current release"
	}
	fmt.Fprintf(stdout, "Enrolled GEOFlow instance %s at %s using %s.\n", *instanceID, *instanceRoot, version)
	composePrefix := fmt.Sprintf(
		"docker compose --env-file %s --env-file %s -f %s",
		shellQuote(result.Instance.Root+"/.env.prod"),
		shellQuote(result.Instance.EnvironmentFile),
		shellQuote(result.Instance.ComposeFile),
	)
	fmt.Fprintf(stdout, "Before handover, confirm every GEOFlow queue is idle. Pending jobs stored in the legacy Redis container may be lost when that container stops.\n")
	fmt.Fprintf(stdout, "Complete the planned handover. The first command stops the standard production project before its database volume is attached to the signed deployment:\n%s down\n%s up -d\n", composePrefix, composePrefix)
	fmt.Fprintf(stdout, "Then verify it:\ngeoflow-updater doctor --instance %s\n", *instanceID)
	return 0
}

func shellQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\r\n'\"\\$`!&;|<>()[]{}*?") {
		return value
	}

	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (app App) doctor(ctx context.Context, arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	instanceID := flags.String("instance", "primary", "managed instance identifier")
	jsonOutput := flags.Bool("json", false, "emit a stable JSON report")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "doctor accepts no positional arguments")
		return 2
	}
	if app.Doctor == nil {
		fmt.Fprintln(stderr, "doctor service is unavailable")
		return 1
	}
	report := app.Doctor.Run(ctx, *instanceID)
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(stderr, "encode doctor report: %v\n", err)
			return 1
		}
	} else {
		fmt.Fprintf(stdout, "GEOFlow Updater doctor: %s\n", report.Status)
		for _, check := range report.Checks {
			fmt.Fprintf(stdout, "[%s] %s: %s\n", check.Status, check.ID, check.Message)
		}
	}
	if report.Status == doctor.StatusFail {
		return 1
	}

	return 0
}

func (app App) serve(ctx context.Context, arguments []string, stderr io.Writer) int {
	if len(arguments) != 0 {
		fmt.Fprintln(stderr, "serve accepts no arguments")
		return 2
	}
	if app.Serve == nil {
		fmt.Fprintln(stderr, "agent server is unavailable")
		return 1
	}
	if err := app.Serve(ctx); err != nil {
		fmt.Fprintf(stderr, "agent server failed: %v\n", err)
		return 1
	}

	return 0
}

func (app App) usage(writer io.Writer) {
	fmt.Fprintln(writer, "Usage: geoflow-updater <enroll|doctor|serve|version>")
}
