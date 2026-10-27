package evaluator

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/skill-up/internal/agent"
	"github.com/alibaba/skill-up/internal/config"
	"github.com/alibaba/skill-up/internal/judge"
	"github.com/alibaba/skill-up/internal/runtime"
)

// Integration tests for the named-metrics path through the real evaluator: a
// script judge is executed as a child process by executeCase, its
// invocation-bound result file is collected, and the resulting envelope reaches
// the grouped outcome and the derived group state.

func metricsScript(t *testing.T, dir string, body string) string {
	t.Helper()
	name := "score.sh"
	if goruntime.GOOS == windowsGOOS {
		name = "score.ps1"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

const (
	posixScore = `#!/bin/sh
cat > "$EVAL_METRICS_RESULT_PATH" <<JSON
{"schema_version":1,"invocation_id":"$EVAL_METRICS_INVOCATION_ID","metrics":[{"metric_id":"correctness","availability":"AVAILABLE","value":0,"evidence":["zero is valid"]},{"metric_id":"completeness","availability":"AVAILABLE","value":0.75,"evidence":["ok"]}]}
JSON
echo "scored"
exit 0
`
	powershellScore = "$body = '{\"schema_version\":1,\"invocation_id\":\"' + $env:EVAL_METRICS_INVOCATION_ID + '\",\"metrics\":[{\"metric_id\":\"correctness\",\"availability\":\"AVAILABLE\",\"value\":0,\"evidence\":[\"zero is valid\"]},{\"metric_id\":\"completeness\",\"availability\":\"AVAILABLE\",\"value\":0.75,\"evidence\":[\"ok\"]}]}'\n" +
		"Set-Content -LiteralPath $env:EVAL_METRICS_RESULT_PATH -Value $body\nWrite-Output scored\nexit 0\n"
)

// sleepPosix and sleepWindows outlive the judge timeout so a later cancellation
// or deadline lands while the member is still running.
const (
	sleepPosix   = "#!/bin/sh\nsleep 5\nexit 0\n"
	sleepWindows = "Start-Sleep -Seconds 5\nexit 0\n"
)

func scoreRegistry() []config.MetricConfig {
	return []config.MetricConfig{{ID: "correctness", Required: true}, {ID: "completeness"}}
}

// newScratchRuntime builds a none runtime on a path the test removes best-effort,
// so a lingering Windows grandchild cannot block harness cleanup.
func newScratchRuntime(t *testing.T, dir string) runtime.Runtime {
	t.Helper()
	rt, err := runtime.NewRuntime(runtime.Config{Type: "none", WorkspaceDir: dir})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	if err := rt.Create(context.Background()); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func newScriptRuntime(t *testing.T) runtime.Runtime {
	t.Helper()
	rt, err := runtime.NewRuntime(runtime.Config{Type: "none", WorkspaceDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	if err := rt.Create(context.Background()); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func TestExecuteCaseSingleScriptJudgePersistsMetrics(t *testing.T) {
	dir := t.TempDir()
	script := metricsScript(t, dir, pick(posixScore, powershellScore))
	limit := 30
	member := config.JudgeConfig{
		ID: "scorer", Type: "script", ScriptPath: script,
		TimeoutSeconds: &limit, Metrics: scoreRegistry(),
	}
	ag := &mockAgent{name: "test", output: "done"}
	outputDir := t.TempDir()
	e := newTestEvaluator(EvalOptions{
		Agent: ag, OutputDir: outputDir,
		EvalCfg: &config.EvalConfig{Environment: config.Environment{Type: "none"}, Judge: member},
	})
	rt := newScriptRuntime(t)
	result := e.executeCase(context.Background(), &config.CaseConfig{ID: "m1", Input: config.Input{Prompt: "hello"}}, "with_skill", rt, nil)

	if result.Status != judge.StatusPass {
		t.Fatalf("status = %s, want PASS (%v)", result.Status, result.Error)
	}
	if result.Grading == nil || result.Grading.Metrics == nil {
		t.Fatalf("expected a collected envelope, got %+v", result.Grading)
	}
	env := result.Grading.Metrics
	if env.Coverage.Planned != 2 || env.Coverage.Available != 2 {
		t.Fatalf("coverage = %+v", env.Coverage)
	}
	if env.Quality != judge.QualityNotConfigured {
		t.Fatalf("quality = %s, want NOT_CONFIGURED (valid scores, no threshold decision)", env.Quality)
	}
	var zero, partial bool
	for _, metric := range env.Metrics {
		if metric.Value == nil {
			t.Fatalf("slot %s has no value", metric.MetricID)
		}
		if metric.MetricID == "correctness" && *metric.Value == 0 {
			zero = true
		}
		if metric.MetricID == "completeness" && *metric.Value == 0.75 {
			partial = true
		}
	}
	if !zero || !partial {
		t.Fatalf("collected values wrong: %+v", env.Metrics)
	}
}

func TestExecuteCaseMultiJudgeDerivesGroupStateFromMembers(t *testing.T) {
	dir := t.TempDir()
	script := metricsScript(t, dir, pick(posixScore, powershellScore))
	limit := 30
	members := []config.JudgeConfig{
		{ID: "a", Type: "script", ScriptPath: script, TimeoutSeconds: &limit, Metrics: scoreRegistry()},
		{ID: "b", Type: "script", ScriptPath: script, TimeoutSeconds: &limit, Metrics: scoreRegistry()},
	}
	ag := &mockAgent{name: "test", output: "done"}
	e := newTestEvaluator(EvalOptions{
		Agent: ag, OutputDir: t.TempDir(),
		EvalCfg: &config.EvalConfig{Environment: config.Environment{Type: "none"}, Judges: &members},
	})
	rt := newScriptRuntime(t)
	result := e.executeCase(context.Background(), &config.CaseConfig{ID: "multi", Input: config.Input{Prompt: "hello"}}, "with_skill", rt, nil)

	if len(result.JudgeResults) != 2 {
		t.Fatalf("outcomes = %d", len(result.JudgeResults))
	}
	for _, outcome := range result.JudgeResults {
		if outcome.Metrics == nil || outcome.Metrics.Coverage.Available != 2 {
			t.Fatalf("member %s envelope = %+v", outcome.ID, outcome.Metrics)
		}
		if outcome.Execution == nil || outcome.Execution.Completion != judge.CompletionFinished ||
			outcome.Execution.Stage != judge.StageFinished {
			t.Fatalf("member %s execution = %+v", outcome.ID, outcome.Execution)
		}
	}
	if result.GroupCompletion != judge.CompletionFinished || result.GroupCause != judge.CauseNone {
		t.Fatalf("group = %s/%s, want COMPLETED/none", result.GroupCompletion, result.GroupCause)
	}
	if result.Status != judge.StatusPass {
		t.Fatalf("compat status = %s, want PASS", result.Status)
	}
}

// TestExecuteCaseParentCancellationStopsRemainingMembers drives the real
// grouped path: the first member completes and is accepted, the case context is
// cancelled while the second member is still running, so one member is
// INTERRUPTED(STARTED) and there is no third member left to invent. A cancelled
// accepted result must keep its verdict and its scores.
func TestExecuteCaseCancellationKeepsAcceptedResult(t *testing.T) {
	if goruntime.GOOS == windowsGOOS {
		// The accepted-then-cancelled ordering depends on the first member finishing
		// before the timer fires, which is not deterministic under Windows process
		// startup costs; the deterministic interruption case is covered by
		// TestExecuteCaseCancellationInterruptsRunningMember.
		t.Skip("timing-sensitive accepted-retention ordering; POSIX only")
	}
	scratch, err := os.MkdirTemp("", "skill-up-cancel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	fast := metricsScript(t, scratch, pick(posixScore, powershellScore))
	slow := metricsScript(t, scratch, pick(sleepPosix, sleepWindows))
	five := 30
	members := []config.JudgeConfig{
		{ID: "fast", Type: "script", ScriptPath: fast, TimeoutSeconds: &five, Metrics: scoreRegistry()},
		{ID: "slow", Type: "script", ScriptPath: slow, TimeoutSeconds: &five, Metrics: scoreRegistry()},
	}
	ag := &mockAgent{name: "test", output: "done"}
	e := newTestEvaluator(EvalOptions{
		Agent: ag, OutputDir: t.TempDir(),
		EvalCfg: &config.EvalConfig{Environment: config.Environment{Type: "none"}, Judges: &members},
	})
	rt := newScratchRuntime(t, scratch)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1500 * time.Millisecond)
		cancel()
	}()
	result := e.executeCase(ctx, &config.CaseConfig{ID: "cancel", Input: config.Input{Prompt: "hello"}}, "with_skill", rt, nil)

	if len(result.JudgeResults) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(result.JudgeResults))
	}
	first, second := result.JudgeResults[0], result.JudgeResults[1]
	if first.Execution == nil || first.Execution.Completion != judge.CompletionFinished ||
		first.Status != judge.StatusPass || first.Metrics == nil || first.Metrics.Coverage.Available != 2 {
		t.Fatalf("accepted member was not retained: status=%s execution=%+v metrics=%+v", first.Status, first.Execution, first.Metrics)
	}
	if second.Execution == nil || second.Execution.Completion != judge.CompletionInterrupted ||
		second.Execution.Stage != judge.StageStarted || second.Execution.Cause != judge.CauseCancellation {
		t.Fatalf("interrupted member = %+v", second.Execution)
	}
	for _, metric := range second.Metrics.Metrics {
		if metric.Availability != judge.NotProduced || metric.Value != nil {
			t.Fatalf("stopped member slot = %+v, want NOT_PRODUCED with no value", metric)
		}
	}
	if result.GroupCompletion != judge.CompletionInterrupted || result.GroupCause != judge.CauseCancellation {
		t.Fatalf("group = %s/%s, want INTERRUPTED/cancellation", result.GroupCompletion, result.GroupCause)
	}
	if result.Status != judge.StatusError {
		t.Fatalf("compat status = %s, want ERROR for an interrupted member", result.Status)
	}
}

// TestRunMultipleJudgesCancellationBeforeInvocation drives the real grouped loop
// with a case context that is already cancelled, so every member takes the
// never-invoked branch: status SKIP for compatibility, INTERRUPTED completion with
// stage NOT_INVOKED, cause cancellation, and each planned slot NOT_PRODUCED.
func TestRunMultipleJudgesCancellationBeforeInvocation(t *testing.T) {
	dir := t.TempDir()
	script := metricsScript(t, dir, pick(posixScore, powershellScore))
	limit := 30
	members := []config.JudgeConfig{
		{ID: "a", Type: "script", ScriptPath: script, TimeoutSeconds: &limit, Metrics: scoreRegistry()},
		{ID: "b", Type: "script", ScriptPath: script, TimeoutSeconds: &limit, Metrics: scoreRegistry()},
	}
	observer := &SignalObserver{}
	observer.MarkSignalObserved()
	ctx, cancel := context.WithCancel(ContextWithSignalObserver(context.Background(), observer))
	cancel()
	ag := &mockAgent{name: "test", output: "done"}
	e := newTestEvaluator(EvalOptions{
		Agent: ag, OutputDir: t.TempDir(),
		EvalCfg: &config.EvalConfig{Environment: config.Environment{Type: "none"}, Judges: &members},
	})
	rt := newScriptRuntime(t)
	result := &EvalResult{SessionResult: &agent.SessionResult{}, CaseID: "pending", CaseName: "pending"}
	out := e.runMultipleJudges(ctx, rt, &config.CaseConfig{ID: "pending", Input: config.Input{Prompt: "hello"}},
		"with_skill", config.JudgePlan{Judges: members, Multi: true}, 1, ag, judge.Input{CaseID: "pending"}, result)

	if len(out.JudgeResults) != 2 {
		t.Fatalf("outcomes = %d, want 2: %s", len(out.JudgeResults), describeOutcomes(out.JudgeResults))
	}
	for _, outcome := range out.JudgeResults {
		if outcome.Status != judge.StatusSkip {
			t.Fatalf("compat status = %s, want SKIP", outcome.Status)
		}
		if outcome.Execution == nil || outcome.Execution.Stage != judge.StageNotInvoked ||
			outcome.Execution.Completion != judge.CompletionInterrupted ||
			outcome.Execution.Cause != judge.CauseCancellation ||
			outcome.Execution.CancelSource != judge.CancelOSSignal {
			t.Fatalf("execution = %+v", outcome.Execution)
		}
		if outcome.Metrics == nil || outcome.Metrics.Coverage.Planned != 2 ||
			outcome.Metrics.Coverage.NotProduced != 2 {
			t.Fatalf("metrics = %+v", outcome.Metrics)
		}
		for _, metric := range outcome.Metrics.Metrics {
			if metric.Availability != judge.NotProduced || metric.Value != nil {
				t.Fatalf("slot = %+v, want NOT_PRODUCED with no value", metric)
			}
		}
		if outcome.Metrics.Quality != judge.QualityNotAssessed {
			t.Fatalf("quality = %s, want NOT_ASSESSED", outcome.Metrics.Quality)
		}
	}
	if out.GroupCompletion != judge.CompletionInterrupted || out.GroupCause != judge.CauseCancellation {
		t.Fatalf("group = %s/%s, want INTERRUPTED/cancellation", out.GroupCompletion, out.GroupCause)
	}
	if out.Status != judge.StatusError {
		t.Fatalf("group verdict = %s, want ERROR (all_required unchanged)", out.Status)
	}
	// The #309 denominator stays put: two member assertions, neither passing.
	if out.Grading == nil || out.Grading.Summary.Total != 2 || out.Grading.Summary.Passed != 0 {
		t.Fatalf("compat grading summary = %+v", out.Grading)
	}
}

func describeOutcomes(outcomes []JudgeOutcome) string {
	parts := make([]string, 0, len(outcomes))
	for _, o := range outcomes {
		stage, completion, cause := "?", "?", "?"
		if o.Execution != nil {
			stage, completion, cause = string(o.Execution.Stage), string(o.Execution.Completion), string(o.Execution.Cause)
		}
		parts = append(parts, o.ID+":"+string(o.Status)+"/"+stage+"/"+completion+"/"+cause)
	}
	return strings.Join(parts, " ")
}

func pick(posix, windows string) string {
	if goruntime.GOOS == windowsGOOS {
		return windows
	}
	return posix
}

// TestExecuteCaseCancellationInterruptsRunningMember is deterministic: the case
// context is already cancelled, so the member in flight never reaches a final
// outcome. Its planned slots must read NOT_PRODUCED (never reached) rather than
// MISSING (omitted by a completed judge), while the legacy SKIP/ERROR verdict
// mapping and the member assertion denominator stay unchanged.
func TestExecuteCaseCancellationInterruptsRunningMember(t *testing.T) {
	scratch, err := os.MkdirTemp("", "skill-up-interrupt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	script := metricsScript(t, scratch, pick(posixScore, powershellScore))
	limit := 30
	members := []config.JudgeConfig{
		{ID: "only", Type: "script", ScriptPath: script, TimeoutSeconds: &limit, Metrics: scoreRegistry()},
	}
	ag := &mockAgent{name: "test", output: "done"}
	e := newTestEvaluator(EvalOptions{
		Agent: ag, OutputDir: t.TempDir(),
		EvalCfg: &config.EvalConfig{Environment: config.Environment{Type: "none"}, Judges: &members},
	})
	rt := newScratchRuntime(t, scratch)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := e.executeCase(ctx, &config.CaseConfig{ID: "interrupt", Input: config.Input{Prompt: "hello"}}, "with_skill", rt, nil)

	if len(result.JudgeResults) != 1 {
		t.Fatalf("outcomes = %d", len(result.JudgeResults))
	}
	outcome := result.JudgeResults[0]
	if outcome.Execution == nil || outcome.Execution.Completion != judge.CompletionInterrupted ||
		outcome.Execution.Cause != judge.CauseCancellation {
		t.Fatalf("execution = %+v", outcome.Execution)
	}
	if outcome.Metrics == nil {
		t.Fatal("expected an envelope for the planned slots")
	}
	for _, metric := range outcome.Metrics.Metrics {
		if metric.Availability != judge.NotProduced || metric.Value != nil {
			t.Fatalf("slot = %+v, want NOT_PRODUCED with no value", metric)
		}
	}
	if outcome.Metrics.Quality != judge.QualityNotAssessed {
		t.Fatalf("quality = %s, want NOT_ASSESSED", outcome.Metrics.Quality)
	}
	if result.GroupCompletion != judge.CompletionInterrupted || result.GroupCause != judge.CauseCancellation {
		t.Fatalf("group = %s/%s", result.GroupCompletion, result.GroupCause)
	}
}
