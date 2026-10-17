package tmux

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// matrix/hardened: actual private executables exercise exec.Cmd Start/Wait, not
// mocked Process fields, and never contact the user's tmux server.
func matrixMetricExecutable(t *testing.T, name string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), name)
	body := "#!/bin/sh\nprintf 'metric-out\\n'\nprintf 'metric-err\\n' >&2\n" +
		"i=0; while [ \"$i\" -lt 2000 ]; do i=$((i+1)); done\n" +
		"exit \"${MATRIX_METRIC_EXIT:-0}\"\n"
	if err := os.WriteFile(bin, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func matrixMetricCommand(bin, category string, exitCode int) *exec.Cmd {
	cmd := exec.Command(bin, "-u", "-L", "private-test-socket", category, "private-test-session")
	cmd.Env = append(os.Environ(), "MATRIX_METRIC_EXIT="+strconv.Itoa(exitCode))
	return cmd
}

func matrixMetricCategory(t *testing.T, category string) SubprocessCategoryMetrics {
	t.Helper()
	for _, metric := range SubprocessMetrics().Categories {
		if metric.Category == category {
			return metric
		}
	}
	t.Fatalf("missing fixed category %q", category)
	return SubprocessCategoryMetrics{}
}

func matrixAssertCompletedMetric(t *testing.T, before, after SubprocessCategoryMetrics, cmd *exec.Cmd, wantError bool) {
	t.Helper()
	wantErrors := uint64(0)
	if wantError {
		wantErrors = 1
	}
	if after.Started-before.Started != 1 || after.Completed-before.Completed != 1 ||
		after.Errors-before.Errors != wantErrors || after.AsyncStarted != before.AsyncStarted {
		t.Fatalf("incorrect completed counters: before=%+v after=%+v", before, after)
	}
	if after.TotalWallNS <= before.TotalWallNS {
		t.Fatal("completed wrapper did not record elapsed wall time")
	}
	if cmd.ProcessState == nil {
		t.Fatal("successful Start did not provide waited child state")
	}
	wantCPU := uint64(cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime())
	if got := after.ChildCPUNS - before.ChildCPUNS; got != wantCPU {
		t.Fatalf("child CPU delta=%d, waited user+system=%d", got, wantCPU)
	}
	if after.ChildCPUObserved-before.ChildCPUObserved != 1 {
		t.Fatal("waited child CPU observation coverage was not counted")
	}
}

func TestMatrixSubprocessMetricsRunCompletion(t *testing.T) {
	bin := matrixMetricExecutable(t, "tmux")
	for _, exitCode := range []int{0, 7} {
		before := matrixMetricCategory(t, "has-session")
		starts := SubprocessStarts()
		cmd := matrixMetricCommand(bin, "has-session", exitCode)
		if got := matrixMetricCategory(t, "has-session"); !reflect.DeepEqual(got, before) {
			t.Fatal("constructing exec.Cmd changed counters")
		}
		err := commandRun(cmd)
		if (err != nil) != (exitCode != 0) {
			t.Fatalf("exit %d returned %v", exitCode, err)
		}
		matrixAssertCompletedMetric(t, before, matrixMetricCategory(t, "has-session"), cmd, exitCode != 0)
		if SubprocessStarts() != starts+1 {
			t.Fatal("SubprocessStarts lost its actual-start contract")
		}
	}
}

func TestMatrixSubprocessMetricsOutputCompletionAndReuse(t *testing.T) {
	bin := matrixMetricExecutable(t, "tmux")
	for _, exitCode := range []int{0, 9} {
		before := matrixMetricCategory(t, "capture-pane")
		cmd := matrixMetricCommand(bin, "capture-pane", exitCode)
		out, err := commandOutput(cmd)
		if string(out) != "metric-out\n" || (err != nil) != (exitCode != 0) {
			t.Fatalf("Output changed its result: output=%q error=%v", out, err)
		}
		if exitErr, ok := err.(*exec.ExitError); ok && string(exitErr.Stderr) != "metric-err\n" {
			t.Fatalf("Output lost captured error stderr: %q", exitErr.Stderr)
		}
		after := matrixMetricCategory(t, "capture-pane")
		matrixAssertCompletedMetric(t, before, after, cmd, exitCode != 0)
		starts := SubprocessStarts()
		if _, err := commandOutput(cmd); err == nil {
			t.Fatal("reusing Output unexpectedly launched another child")
		}
		if _, err := commandCombinedOutput(cmd); err == nil {
			t.Fatal("reusing CombinedOutput unexpectedly launched another child")
		}
		if got := matrixMetricCategory(t, "capture-pane"); !reflect.DeepEqual(got, after) || SubprocessStarts() != starts {
			t.Fatal("Output reuse counted its old Process or ProcessState twice")
		}
	}
}

func TestMatrixSubprocessMetricsCombinedOutputCompletionAndReuse(t *testing.T) {
	bin := matrixMetricExecutable(t, "tmux")
	for _, exitCode := range []int{0, 11} {
		before := matrixMetricCategory(t, "list-panes")
		cmd := matrixMetricCommand(bin, "list-panes", exitCode)
		out, err := commandCombinedOutput(cmd)
		if string(out) != "metric-out\nmetric-err\n" || (err != nil) != (exitCode != 0) {
			t.Fatalf("CombinedOutput changed its result: output=%q error=%v", out, err)
		}
		after := matrixMetricCategory(t, "list-panes")
		matrixAssertCompletedMetric(t, before, after, cmd, exitCode != 0)
		if _, err := commandCombinedOutput(cmd); err == nil {
			t.Fatal("reusing CombinedOutput unexpectedly launched another child")
		}
		if got := matrixMetricCategory(t, "list-panes"); !reflect.DeepEqual(got, after) {
			t.Fatal("CombinedOutput reuse counted its old Process or ProcessState twice")
		}
	}
}

func TestMatrixSubprocessMetricsStartFailureAndOutputSetupFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "tmux")
	before := matrixMetricCategory(t, "display-message")
	starts := SubprocessStarts()
	if err := commandRun(matrixMetricCommand(missing, "display-message", 0)); err == nil {
		t.Fatal("Run accepted a missing executable")
	}
	if err := commandStart(matrixMetricCommand(missing, "display-message", 0)); err == nil {
		t.Fatal("Start accepted a missing executable")
	}
	if _, err := commandOutput(matrixMetricCommand(missing, "display-message", 0)); err == nil {
		t.Fatal("Output accepted a missing executable")
	}
	if _, err := commandCombinedOutput(matrixMetricCommand(missing, "display-message", 0)); err == nil {
		t.Fatal("CombinedOutput accepted a missing executable")
	}
	bin := matrixMetricExecutable(t, "tmux")
	outputSetupFailure := matrixMetricCommand(bin, "display-message", 0)
	outputSetupFailure.Stdout = io.Discard
	if _, err := commandOutput(outputSetupFailure); err == nil || outputSetupFailure.Process != nil {
		t.Fatal("Output with existing stdout unexpectedly launched")
	}
	combinedSetupFailure := matrixMetricCommand(bin, "display-message", 0)
	combinedSetupFailure.Stderr = io.Discard
	if _, err := commandCombinedOutput(combinedSetupFailure); err == nil || combinedSetupFailure.Process != nil {
		t.Fatal("CombinedOutput with existing stderr unexpectedly launched")
	}
	if got := matrixMetricCategory(t, "display-message"); !reflect.DeepEqual(got, before) || SubprocessStarts() != starts {
		t.Fatal("pre-Start errors were included in executed-command counters")
	}
}

func TestMatrixSubprocessMetricsAsyncCoverage(t *testing.T) {
	bin := matrixMetricExecutable(t, "tmux")
	before := matrixMetricCategory(t, "attach-session")
	starts := SubprocessStarts()
	cmd := matrixMetricCommand(bin, "attach-session", 13)
	if err := commandStart(cmd); err != nil {
		t.Fatal(err)
	}
	afterStart := matrixMetricCategory(t, "attach-session")
	want := before
	want.Started++
	want.AsyncStarted++
	if !reflect.DeepEqual(afterStart, want) || SubprocessStarts() != starts+1 {
		t.Fatalf("async start changed timing/completion counters: got=%+v want=%+v", afterStart, want)
	}
	if err := commandStart(cmd); err == nil {
		t.Fatal("second Start unexpectedly succeeded")
	}
	if _, err := commandOutput(cmd); err == nil {
		t.Fatal("Output on started Cmd unexpectedly succeeded")
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("nonzero async child unexpectedly succeeded")
	}
	if got := matrixMetricCategory(t, "attach-session"); !reflect.DeepEqual(got, afterStart) {
		t.Fatal("direct async Wait invented completion coverage")
	}
}

func TestMatrixSubprocessMetricsCommandBoundaryAndPrivacy(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"-u", "-L", "capture-pane", "list-panes", "-t", "private-session"}, "list-panes"},
		{[]string{"-u", "-L", "private-socket", "-S", "/private/socket-path", "capture-pane", "-S", "-2000"}, "capture-pane"},
		{[]string{"-uv", "-Lprivate-socket", "-S/private/path", "capture-pane", "-t", "private-target", "-S", "-2000"}, "capture-pane"},
		{[]string{"-CC", "-u", "-f", "capture-pane", "-T", "features", "attach-session"}, "attach-session"},
		{[]string{"-u", "--", "has-session", "-t", "private-target"}, "has-session"},
		{[]string{"set", "-g", "private-option", "private-value", ";", "capture-pane"}, "batch"},
		{[]string{"-u", "-L", ";", "capture-pane", "-S", "-2000"}, "capture-pane"},
		{[]string{"capture-pane", "-S", "-2000", ";", "display-message", "private-marker"}, "batch"},
		{[]string{"send-keys", "-l", "\\;"}, "send-keys"},
		{[]string{"bind", "-n", "private-key"}, "bind-key"},
		{[]string{"unbind", "-n", "private-key"}, "unbind-key"},
		{[]string{"setw", "-t", "private-target"}, "set-option"},
		{[]string{"-V"}, "version"},
		{[]string{"private-unrecognized-command", "capture-pane"}, "other"},
		{[]string{"-private-unknown-flag", "capture-pane"}, "other"},
		{[]string{"-L"}, "other"},
		{nil, "other"},
	}
	for _, tc := range tests {
		cmd := exec.Command("/private/fake/tmux", tc.args...)
		got := subprocessCommandCategory(cmd)
		if got < 0 || subprocessCategoryNames[got] != tc.want {
			t.Fatalf("args=%q category=%d want=%q", tc.args, got, tc.want)
		}
	}
	if got := subprocessCommandCategory(exec.Command("/private/fake/other", "capture-pane")); got != -1 {
		t.Fatal("non-tmux executable was classified as native tmux")
	}
	encoded, err := json.Marshal(SubprocessMetrics())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-session", "private-socket", "private-target", "/private/", "metric-out", "metric-err"} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatalf("snapshot exposed command data %q", private)
		}
	}
	snapshot := SubprocessMetrics()
	if len(snapshot.Categories) != len(subprocessCategoryNames) || !strings.Contains(snapshot.TimingCoverage, "Start is start-only") ||
		!strings.Contains(snapshot.ChildCPUScope, "excludes parent") {
		t.Fatalf("snapshot is unbounded or omits coverage: %+v", snapshot)
	}
	// Mutating the returned value must not mutate retained names or counters.
	snapshot.Categories[0].Category = "private-mutated-category"
	snapshot.Categories[0].Started = 0
	if SubprocessMetrics().Categories[0].Category != "other" {
		t.Fatal("snapshot aliases retained state")
	}
}

func TestMatrixSubprocessMetricsNonTmuxAndConcurrentSnapshots(t *testing.T) {
	bin := matrixMetricExecutable(t, "not-tmux")
	before := SubprocessMetrics()
	starts := SubprocessStarts()
	if err := commandRun(matrixMetricCommand(bin, "capture-pane", 0)); err != nil {
		t.Fatal(err)
	}
	if got := SubprocessMetrics(); !reflect.DeepEqual(got, before) || SubprocessStarts() != starts {
		t.Fatal("non-tmux command changed native counters")
	}
	bin = matrixMetricExecutable(t, "tmux")
	beforeCategory := matrixMetricCategory(t, "list-clients")
	const commands = 12
	var wg sync.WaitGroup
	errors := make(chan error, commands)
	for i := 0; i < commands; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := commandOutput(matrixMetricCommand(bin, "list-clients", 0))
			errors <- err
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	previous := beforeCategory
snapshotLoop:
	for {
		select {
		case <-done:
			break snapshotLoop
		default:
			got := matrixMetricCategory(t, "list-clients")
			if got.Started < previous.Started || got.AsyncStarted < previous.AsyncStarted || got.Completed < previous.Completed ||
				got.Errors < previous.Errors || got.TotalWallNS < previous.TotalWallNS || got.ChildCPUNS < previous.ChildCPUNS ||
				got.ChildCPUObserved < previous.ChildCPUObserved {
				t.Fatal("concurrent snapshot decreased a counter")
			}
			previous = got
		}
	}
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	after := matrixMetricCategory(t, "list-clients")
	if after.Started-beforeCategory.Started != commands || after.Completed-beforeCategory.Completed != commands ||
		after.ChildCPUObserved-beforeCategory.ChildCPUObserved != commands || after.Errors != beforeCategory.Errors ||
		after.AsyncStarted != beforeCategory.AsyncStarted {
		t.Fatalf("lost concurrent command metrics: before=%+v after=%+v", beforeCategory, after)
	}
}
