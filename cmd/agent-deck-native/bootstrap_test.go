package main

// matrix/hardened: startup tests prove absence and a compatible full handshake
// before spawning or reusing; injected brokers never own real processes.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/google/uuid"
)

var bootstrapTestAbsent = errors.New("fixture endpoint does not exist")

func bootstrapTestConfig(t *testing.T) options {
	t.Helper()
	return options{StateDir: filepath.Join(t.TempDir(), "private manager"), Pipe: pipePrefix + "bootstrap-" + uuid.NewString()}
}

func bootstrapTestResponse(config options) terminalhost.Response {
	return terminalhost.Response{OK: true, HostPID: 42, RunID: uuid.NewString(), ManagerStateDir: config.StateDir, ManagerVersion: version, ProtocolVersion: nativeManagerProtocol}
}

func bootstrapTestIsAbsent(err error) bool { return errors.Is(err, bootstrapTestAbsent) }

func TestNativeBootstrapReusesCompatibleManagerWithoutLaunch(t *testing.T) {
	config := bootstrapTestConfig(t)
	want := bootstrapTestResponse(config)
	probes, spawns := 0, 0
	got, err := ensureNativeManagerWith(context.Background(), config, nativeBootstrapDeps{
		probe: func(ctx context.Context, pipe string) (terminalhost.Response, error) {
			probes++
			if pipe != config.Pipe || ctx.Err() != nil {
				t.Fatalf("probe received the wrong namespace or expired context")
			}
			return want, nil
		},
		spawn:  func(context.Context, options) (uint32, error) { spawns++; return 0, errors.New("must not launch") },
		absent: bootstrapTestIsAbsent,
	})
	if err != nil || got.RunID != want.RunID || probes != 1 || spawns != 0 {
		t.Fatalf("compatible reuse = %+v, %v; probes=%d spawns=%d", got, err, probes, spawns)
	}
}

func TestNativeBootstrapLaunchesOnlyAfterConfirmedAbsence(t *testing.T) {
	config := bootstrapTestConfig(t)
	want := bootstrapTestResponse(config)
	probes, spawns := 0, 0
	got, err := ensureNativeManagerWith(context.Background(), config, nativeBootstrapDeps{
		probe: func(context.Context, string) (terminalhost.Response, error) {
			probes++
			if probes < 3 {
				return terminalhost.Response{}, fmt.Errorf("dial: %w", bootstrapTestAbsent)
			}
			return want, nil
		},
		spawn: func(ctx context.Context, received options) (uint32, error) {
			spawns++
			if probes != 1 || received.StateDir != config.StateDir || received.Pipe != config.Pipe || ctx.Err() != nil {
				t.Fatalf("launch occurred without exact, live namespace absence: %+v", received)
			}
			return 42, nil
		},
		absent: bootstrapTestIsAbsent,
	})
	if err != nil || got.RunID != want.RunID || probes != 3 || spawns != 1 {
		t.Fatalf("absent launch = %+v, %v; probes=%d spawns=%d", got, err, probes, spawns)
	}
}

func TestNativeBootstrapReusesConcurrentWinner(t *testing.T) {
	for _, spawnFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("broker_failure_%t", spawnFailure), func(t *testing.T) {
			config := bootstrapTestConfig(t)
			winner := bootstrapTestResponse(config)
			winner.HostPID = 77 // The losing spawned PID is never an identity to kill.
			probes, spawns := 0, 0
			got, err := ensureNativeManagerWith(context.Background(), config, nativeBootstrapDeps{
				probe: func(context.Context, string) (terminalhost.Response, error) {
					probes++
					if probes == 1 {
						return terminalhost.Response{}, bootstrapTestAbsent
					}
					return winner, nil
				},
				spawn: func(context.Context, options) (uint32, error) {
					spawns++
					if spawnFailure {
						return 0, errors.New("fixture broker lost the race")
					}
					return 42, nil
				},
				absent: bootstrapTestIsAbsent,
			})
			if err != nil || got.HostPID != winner.HostPID || got.RunID != winner.RunID || probes != 2 || spawns != 1 {
				t.Fatalf("concurrent winner = %+v, %v; probes=%d spawns=%d", got, err, probes, spawns)
			}
		})
	}
}

func TestNativeBootstrapNeverLaunchesAfterUncertainProbe(t *testing.T) {
	for _, probeError := range []error{errors.New("permission denied"), errors.New("all pipe instances busy"), context.DeadlineExceeded, errors.New("malformed response"), errors.New("server SID mismatch")} {
		t.Run(probeError.Error(), func(t *testing.T) {
			config := bootstrapTestConfig(t)
			spawns := 0
			_, err := ensureNativeManagerWith(context.Background(), config, nativeBootstrapDeps{
				probe: func(context.Context, string) (terminalhost.Response, error) {
					return terminalhost.Response{}, probeError
				},
				spawn:  func(context.Context, options) (uint32, error) { spawns++; return 42, nil },
				absent: bootstrapTestIsAbsent,
			})
			if !errors.Is(err, probeError) || spawns != 0 {
				t.Fatalf("uncertain endpoint authorized launch: err=%v spawns=%d", err, spawns)
			}
		})
	}
}

func TestNativeBootstrapRejectsIncompleteOrForeignHandshake(t *testing.T) {
	cases := map[string]func(*terminalhost.Response){
		"rejected":         func(r *terminalhost.Response) { r.OK = false },
		"zero_pid":         func(r *terminalhost.Response) { r.HostPID = 0 },
		"missing_run":      func(r *terminalhost.Response) { r.RunID = "" },
		"malformed_run":    func(r *terminalhost.Response) { r.RunID = "not-a-uuid" },
		"nil_run":          func(r *terminalhost.Response) { r.RunID = uuid.Nil.String() },
		"noncanonical_run": func(r *terminalhost.Response) { r.RunID = strings.ToUpper(r.RunID) },
		"old_version":      func(r *terminalhost.Response) { r.ManagerVersion = "0.1.0+matrix.prototype" },
		"old_protocol":     func(r *terminalhost.Response) { r.ProtocolVersion-- },
		"missing_state":    func(r *terminalhost.Response) { r.ManagerStateDir = "" },
		"relative_state":   func(r *terminalhost.Response) { r.ManagerStateDir = "relative/manager" },
		"nul_state":        func(r *terminalhost.Response) { r.ManagerStateDir += "\x00" },
		"foreign_state": func(r *terminalhost.Response) {
			r.ManagerStateDir = filepath.Join(filepath.Dir(r.ManagerStateDir), "foreign-manager")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := bootstrapTestConfig(t)
			response := bootstrapTestResponse(config)
			// Ensure this canonical UUID has letters so uppercase changes it.
			response.RunID = "abcdefab-1234-4234-8234-abcdefabcdef"
			mutate(&response)
			spawns := 0
			_, err := ensureNativeManagerWith(context.Background(), config, nativeBootstrapDeps{
				probe:  func(context.Context, string) (terminalhost.Response, error) { return response, nil },
				spawn:  func(context.Context, options) (uint32, error) { spawns++; return 42, nil },
				absent: bootstrapTestIsAbsent,
			})
			if err == nil || spawns != 0 {
				t.Fatalf("invalid existing handshake authorized replacement: err=%v spawns=%d response=%+v", err, spawns, response)
			}
		})
	}
}

func TestNativeBootstrapCancellationDoesNotLaunchOrRetry(t *testing.T) {
	t.Run("before_probe", func(t *testing.T) {
		config := bootstrapTestConfig(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		_, err := ensureNativeManagerWith(ctx, config, nativeBootstrapDeps{
			probe: func(context.Context, string) (terminalhost.Response, error) {
				calls++
				return terminalhost.Response{}, bootstrapTestAbsent
			},
			spawn:  func(context.Context, options) (uint32, error) { calls++; return 42, nil },
			absent: bootstrapTestIsAbsent,
		})
		if !errors.Is(err, context.Canceled) || calls != 0 {
			t.Fatalf("canceled startup touched endpoint: err=%v calls=%d", err, calls)
		}
	})
	t.Run("after_absence", func(t *testing.T) {
		config := bootstrapTestConfig(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		spawns := 0
		_, err := ensureNativeManagerWith(ctx, config, nativeBootstrapDeps{
			probe: func(context.Context, string) (terminalhost.Response, error) {
				cancel()
				return terminalhost.Response{}, bootstrapTestAbsent
			},
			spawn:  func(context.Context, options) (uint32, error) { spawns++; return 42, nil },
			absent: bootstrapTestIsAbsent,
		})
		if !errors.Is(err, context.Canceled) || spawns != 0 {
			t.Fatalf("canceled absence authorized spawn: err=%v spawns=%d", err, spawns)
		}
	})
	t.Run("after_spawn", func(t *testing.T) {
		config := bootstrapTestConfig(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		probes, spawns := 0, 0
		_, err := ensureNativeManagerWith(ctx, config, nativeBootstrapDeps{
			probe: func(context.Context, string) (terminalhost.Response, error) {
				probes++
				return terminalhost.Response{}, bootstrapTestAbsent
			},
			spawn:  func(context.Context, options) (uint32, error) { spawns++; cancel(); return 42, nil },
			absent: bootstrapTestIsAbsent,
		})
		if !errors.Is(err, context.Canceled) || probes != 1 || spawns != 1 {
			t.Fatalf("canceled broker request retried: err=%v probes=%d spawns=%d", err, probes, spawns)
		}
	})
}

func TestNativeBootstrapStartupDeadlineIsBounded(t *testing.T) {
	config := bootstrapTestConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	probes, spawns := 0, 0
	_, err := ensureNativeManagerWith(ctx, config, nativeBootstrapDeps{
		probe: func(context.Context, string) (terminalhost.Response, error) {
			probes++
			return terminalhost.Response{}, bootstrapTestAbsent
		},
		spawn:  func(context.Context, options) (uint32, error) { spawns++; return 42, nil },
		absent: bootstrapTestIsAbsent,
	})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second || probes != 2 || spawns != 1 {
		t.Fatalf("unready broker failed to honor caller deadline: err=%v elapsed=%s probes=%d spawns=%d", err, time.Since(start), probes, spawns)
	}
}

func TestNativeBootstrapLaunchFailureIsNotRetried(t *testing.T) {
	config := bootstrapTestConfig(t)
	spawnError := errors.New("fixture broker rejected request")
	spawns, probes := 0, 0
	_, err := ensureNativeManagerWith(context.Background(), config, nativeBootstrapDeps{
		probe: func(context.Context, string) (terminalhost.Response, error) {
			probes++
			return terminalhost.Response{}, bootstrapTestAbsent
		},
		spawn:  func(context.Context, options) (uint32, error) { spawns++; return 0, spawnError },
		absent: bootstrapTestIsAbsent,
	})
	if !errors.Is(err, spawnError) || spawns != 1 || probes != 2 {
		t.Fatalf("broker rejection was retried: err=%v spawns=%d probes=%d", err, spawns, probes)
	}
}

func TestNativeBootstrapUncertainPostLaunchEndpointStopsRetries(t *testing.T) {
	for _, spawnFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("broker_failure_%t", spawnFailure), func(t *testing.T) {
			config := bootstrapTestConfig(t)
			probeError := errors.New("fixture endpoint has an unverifiable owner")
			probes, spawns := 0, 0
			_, err := ensureNativeManagerWith(context.Background(), config, nativeBootstrapDeps{
				probe: func(context.Context, string) (terminalhost.Response, error) {
					probes++
					if probes == 1 {
						return terminalhost.Response{}, bootstrapTestAbsent
					}
					return terminalhost.Response{}, probeError
				},
				spawn: func(context.Context, options) (uint32, error) {
					spawns++
					if spawnFailure {
						return 0, errors.New("fixture broker failed")
					}
					return 42, nil
				},
				absent: bootstrapTestIsAbsent,
			})
			if !errors.Is(err, probeError) || probes != 2 || spawns != 1 {
				t.Fatalf("uncertain post-launch endpoint was retried: err=%v probes=%d spawns=%d", err, probes, spawns)
			}
		})
	}
}

func TestNativeBootstrapRejectsInvalidNamespaceBeforeProbe(t *testing.T) {
	config := bootstrapTestConfig(t)
	for _, invalid := range []options{{Pipe: "bad", StateDir: config.StateDir}, {Pipe: config.Pipe, StateDir: "relative"}, {Pipe: config.Pipe, StateDir: config.StateDir + "\x00"}} {
		calls := 0
		_, err := ensureNativeManagerWith(context.Background(), invalid, nativeBootstrapDeps{
			probe: func(context.Context, string) (terminalhost.Response, error) {
				calls++
				return terminalhost.Response{}, bootstrapTestAbsent
			},
			spawn:  func(context.Context, options) (uint32, error) { calls++; return 42, nil },
			absent: bootstrapTestIsAbsent,
		})
		if err == nil || calls != 0 {
			t.Fatalf("invalid namespace performed startup: %+v err=%v calls=%d", invalid, err, calls)
		}
	}
}
