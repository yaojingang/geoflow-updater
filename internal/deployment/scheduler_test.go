package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yaojingang/geoflow-updater/internal/instance"
)

func TestLegacySchedulerWithoutSignalHandlerStopsOnlyAfterChildrenFinish(t *testing.T) {
	for _, children := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "active-child"}[children], func(t *testing.T) {
			running, frozen, killed := true, false, false
			service := Service{StateDir: canonicalTemp(t), Runner: functionRunner(func(_ context.Context, _ io.Reader, out io.Writer, _ string, args ...string) error {
				cmd := strings.Join(args, " ")
				switch {
				case strings.HasSuffix(cmd, "ps --all --quiet scheduler"):
					_, _ = io.WriteString(out, "0123456789ab")
				case strings.Contains(cmd, "{{json .Config.Cmd}}"):
					_, _ = io.WriteString(out, `["php","artisan","schedule:work"]`)
				case strings.Contains(cmd, "{{json .State}}"):
					code := 0
					if killed {
						code = 137
					}
					return json.NewEncoder(out).Encode(schedulerState{Running: running, Pid: 100, StartedAt: "2026-09-09T00:00:00Z", ExitCode: code})
				case strings.Contains(cmd, "--signal=STOP"):
					frozen = true
				case strings.HasPrefix(cmd, "top "):
					if !frozen {
						t.Fatal("scheduler must stop spawning before inspecting children")
					}
					if children {
						_, _ = io.WriteString(out, "PID STAT\n100 T\n101 S\n")
					} else {
						_, _ = io.WriteString(out, "PID STAT\n100 T\n")
					}
				case strings.Contains(cmd, "--signal=KILL"):
					if !frozen || children {
						t.Fatal("attempted to kill an unproven scheduler")
					}
					killed, running = true, false
				case strings.Contains(cmd, "--signal=CONT"):
					frozen = false
				case strings.Contains(cmd, "Running}}|"):
					if running {
						_, _ = io.WriteString(out, "true|0|false")
					} else {
						_, _ = io.WriteString(out, "false|137|false")
					}
				case strings.Contains(cmd, "Running}}"):
					if running {
						_, _ = io.WriteString(out, "true")
					} else {
						_, _ = io.WriteString(out, "false")
					}
				}
				return nil
			})}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			err := service.drainScheduler(ctx, instance.Config{ID: "primary"})
			if children {
				if err == nil || killed || frozen {
					t.Fatalf("active scheduler must be retained and resumed: %v", err)
				}
			} else if err != nil || !killed {
				t.Fatalf("idle PID 1 scheduler ignoring TERM did not exit: %v", err)
			} else if err := service.drainScheduler(context.Background(), instance.Config{ID: "primary"}); err != nil {
				t.Fatalf("repeated drain failed: %v", err)
			}
		})
	}
}

func TestLegacySchedulerIdleRequiresFrozenSpawnerAndExitedChildren(t *testing.T) {
	for _, test := range []struct {
		name, processes string
		idle, invalid   bool
	}{
		{"idle", "PID STAT\n100 T\n", true, false},
		{"running child", "PID STAT\n100 T\n101 S\n", false, false},
		{"zombie child", "PID STAT\n100 T\n101 Z\n", true, false},
		{"zombie plus writer", "PID STAT\n100 T\n101 Z\n102 D\n", false, false},
		{"unfrozen parent", "PID STAT\n100 S\n", false, true},
		{"different parent", "PID STAT\n101 T\n", false, true},
		{"malformed", "PID\n100\n", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			idle, err := legacySchedulerIdle(test.processes, 100)
			if (err != nil) != test.invalid || idle != test.idle {
				t.Fatalf("idle = %v, error = %v", idle, err)
			}
		})
	}
}

func TestLegacySchedulerRejectsUnprovenOrOOMTermination(t *testing.T) {
	for _, oom := range []bool{false, true} {
		service := Service{StateDir: canonicalTemp(t), Runner: functionRunner(func(_ context.Context, _ io.Reader, out io.Writer, _ string, args ...string) error {
			if len(args) == 3 && strings.Contains(args[1], "json .State") {
				return json.NewEncoder(out).Encode(schedulerState{ExitCode: 137, OOMKilled: oom, StartedAt: "2026-09-09T00:00:00Z"})
			}
			t.Fatalf("unexpected mutation: %v", args)
			return nil
		})}
		if err := service.drainLegacyScheduler(context.Background(), instance.Config{ID: "primary"}, "0123456789ab"); err == nil {
			t.Fatal("unproven termination accepted")
		}
	}
}

func TestSchedulerFreezeIntentRecoversAfterServiceCrash(t *testing.T) {
	config := instance.Config{ID: "primary"}
	state := schedulerState{Running: true, Pid: 100, StartedAt: "2026-09-09T00:00:00Z"}
	continued := false
	service := Service{StateDir: canonicalTemp(t), Runner: functionRunner(func(_ context.Context, _ io.Reader, out io.Writer, _ string, args ...string) error {
		cmd := strings.Join(args, " ")
		switch {
		case strings.HasSuffix(cmd, "ps --all --quiet scheduler"):
			_, _ = io.WriteString(out, "0123456789ab")
		case strings.Contains(cmd, "json .State"):
			return json.NewEncoder(out).Encode(state)
		case strings.Contains(cmd, "--signal=CONT"):
			continued = true
		default:
			t.Fatalf("unexpected command: %v", args)
		}
		return nil
	})}
	if err := safeDirectory(service.instanceDirectory("primary"), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(state)
	intent := filepath.Join(service.instanceDirectory("primary"), "legacy-scheduler-freeze-0123456789ab.json")
	proof := filepath.Join(service.instanceDirectory("primary"), "legacy-scheduler-drain-0123456789ab.json")
	writeTest(t, intent, data)
	writeTest(t, proof, data)
	if err := service.resumeFrozenSchedulers(context.Background(), config); err != nil || !continued {
		t.Fatalf("resume freeze: %v", err)
	}
	if _, err := os.Stat(intent); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("freeze intent retained after continuation")
	}
	state.Running, state.ExitCode = false, 137
	if err := service.drainLegacyScheduler(context.Background(), config, "0123456789ab"); err == nil {
		t.Fatal("old idle proof accepted a later abnormal exit")
	}
}

func TestSchedulerUncertainStopAndFailedKillAlwaysThaw(t *testing.T) {
	for _, boundary := range []string{"STOP", "KILL"} {
		t.Run(boundary, func(t *testing.T) {
			frozen, continued := false, false
			state := schedulerState{Running: true, Pid: 100, StartedAt: "2026-09-09T00:00:00Z"}
			service := Service{StateDir: canonicalTemp(t), Runner: functionRunner(func(_ context.Context, _ io.Reader, out io.Writer, _ string, args ...string) error {
				cmd := strings.Join(args, " ")
				switch {
				case strings.Contains(cmd, "json .State"):
					return json.NewEncoder(out).Encode(state)
				case strings.Contains(cmd, "--signal=STOP"):
					frozen = true
					if boundary == "STOP" {
						return errors.New("STOP response lost")
					}
				case strings.HasPrefix(cmd, "top "):
					_, _ = io.WriteString(out, "PID STAT\n100 T\n")
				case strings.Contains(cmd, "--signal=KILL"):
					return errors.New("KILL failed")
				case strings.Contains(cmd, "--signal=CONT"):
					frozen, continued = false, true
				}
				return nil
			})}
			config := instance.Config{ID: "primary"}
			if err := service.drainLegacyScheduler(context.Background(), config, "0123456789ab"); err == nil || frozen || !continued {
				t.Fatalf("uncertain signal did not thaw: %v", err)
			}
			state.Running, state.ExitCode = false, 137
			if err := service.drainLegacyScheduler(context.Background(), config, "0123456789ab"); err == nil {
				t.Fatal("failed kill proof authorized a later abnormal exit")
			}
		})
	}
}
