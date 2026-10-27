package judge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	evalruntime "github.com/alibaba/skill-up/internal/runtime"
)

// DefaultScriptTimeout is the default timeout for script execution (30s).
const DefaultScriptTimeout = 30 * time.Second

// ScriptJudge runs a user-provided evaluation script in the case workspace.
//
// Script contract (design doc):
//   - Working directory: case workspace root
//   - Environment variables: EVAL_TRANSCRIPT_PATH, EVAL_FINAL_MESSAGE, EVAL_EXIT_CODE
//   - Exit code 0 → PASS, non-0 → FAIL
//   - stdout → evaluation evidence, stderr → debug info
type ScriptJudge struct {
	// TimeoutIsError marks a timed-out script as an execution error.
	TimeoutIsError bool
	// ScriptPath is the absolute or workspace-relative path to the evaluation script.
	ScriptPath string

	// TimeoutSeconds overrides DefaultScriptTimeout. 0 means use default.
	TimeoutSeconds int

	// TranscriptPath is the absolute path to the transcript.json file.
	// Injected as EVAL_TRANSCRIPT_PATH environment variable.
	TranscriptPath string

	// Runtime executes the script in the same environment as the evaluated agent.
	// When nil, ScriptJudge creates a none runtime so local execution uses the same path.
	Runtime evalruntime.Runtime

	// MetricSlots is the planned named-metric registry for this judge. Empty means
	// the judge produces no metrics envelope at all.
	MetricSlots []MetricSlot

	// MetricsResultPath is the runner-assigned host path for this invocation's
	// structured result. Set only when the script runs against the local runtime.
	MetricsResultPath string

	// MetricsInvocationID binds the collected file to this exact invocation, so a
	// leftover file from an earlier attempt is never accepted.
	MetricsInvocationID string
}

// Evaluate implements the Judge interface by executing the configured script.
func (j *ScriptJudge) Evaluate(ctx context.Context, in Input) (*Result, error) {
	if j.TranscriptPath == "" {
		log.Println("warning: ScriptJudge.TranscriptPath is empty; EVAL_TRANSCRIPT_PATH will be unset")
	}

	timeout := DefaultScriptTimeout
	if j.TimeoutSeconds > 0 {
		timeout = time.Duration(j.TimeoutSeconds) * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	rt, cleanup, err := j.runtime(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return j.evaluateInRuntime(ctx, rt, in, timeout)
}

func (j *ScriptJudge) runtime(ctx context.Context) (evalruntime.Runtime, func(), error) {
	if j.Runtime != nil {
		return j.Runtime, func() {}, nil
	}
	rt, err := evalruntime.NewRuntime(evalruntime.Config{Type: "none", Delete: true})
	if err != nil {
		return nil, nil, fmt.Errorf("script execution failed: create none runtime: %w", err)
	}
	if err := rt.Create(ctx); err != nil {
		return nil, nil, fmt.Errorf("script execution failed: create none runtime workspace: %w", err)
	}
	return rt, func() { _ = rt.Close() }, nil
}

func (j *ScriptJudge) evaluateInRuntime(ctx context.Context, rt evalruntime.Runtime, in Input, timeout time.Duration) (*Result, error) {
	shell := rt.Shell()
	targetGOOS := shell.GOOS
	plan, err := planScript(j.ScriptPath, shell)
	if err != nil {
		return nil, fmt.Errorf("script execution failed: %w", err)
	}

	remoteDir := judgeTempDir(targetGOOS)
	remoteScript := joinForGOOS(targetGOOS, remoteDir, plan.uploadName)
	if err := rt.UploadFile(ctx, j.ScriptPath, remoteScript); err != nil {
		return nil, fmt.Errorf("script execution failed: upload script judge: %w", err)
	}
	defer func() {
		_, _ = rt.Exec(context.WithoutCancel(ctx), plan.cleanupCommand(remoteDir), evalruntime.ExecOptions{})
	}()

	remoteTranscript := ""
	if j.TranscriptPath != "" {
		remoteTranscript = joinForGOOS(targetGOOS, remoteDir, "transcript.json")
		if err := rt.UploadFile(ctx, j.TranscriptPath, remoteTranscript); err != nil {
			return nil, fmt.Errorf("script execution failed: upload script judge transcript: %w", err)
		}
	}

	command := plan.command(remoteScript)
	cwd := in.WorkspacePath
	if cwd == "" {
		cwd = rt.Workspace()
	}
	// Translate the transcript path into the script interpreter's preferred
	// form (e.g. forward slashes for `.sh` running under Git Bash so POSIX
	// tools can `cat "$EVAL_TRANSCRIPT_PATH"`). Every plan returned by
	// planScript sets envPath (identity on POSIX, ToSlash on Windows .sh),
	// so the function is always safe to invoke.
	transcriptEnv := remoteTranscript
	if remoteTranscript != "" {
		transcriptEnv = plan.envPath(remoteTranscript)
	}
	env := map[string]string{
		"EVAL_TRANSCRIPT_PATH": transcriptEnv,
		"EVAL_FINAL_MESSAGE":   in.FinalMessage,
		"EVAL_EXIT_CODE":       strconv.Itoa(in.ExitCode),
	}
	if len(j.MetricSlots) > 0 && j.MetricsResultPath != "" {
		env["EVAL_METRICS_RESULT_PATH"] = j.MetricsResultPath
		env["EVAL_METRICS_INVOCATION_ID"] = j.MetricsInvocationID
		env["EVAL_METRICS_TEMPLATE"] = MetricsPayloadTemplate(j.MetricsInvocationID)
	}
	result, err := rt.Exec(ctx, command, evalruntime.ExecOptions{
		Cwd:        cwd,
		TimeoutSec: int(timeout.Seconds()),
		Env:        env,
	})
	evidence := strings.TrimSpace(result.Stdout)
	debugInfo := strings.TrimSpace(result.Stderr)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			// A timeout does not invalidate a trustworthy result: if this
			// invocation's own schema-valid file was written before the deadline,
			// the scores are collected while the existing verdict mapping and the
			// timeout record are preserved.
			if j.TimeoutIsError {
				timeoutErr := fmt.Errorf("script timed out after %s: %w", timeout, ctx.Err())
				res := NewErrorResult(timeoutErr, in.TurnsExecuted, in.TurnsTotal)
				j.attachMetrics(res, in, true)
				return res, timeoutErr
			}
			res := j.buildResult(false, "script timed out after "+timeout.String(), debugInfo, in)
			j.attachMetrics(res, in, true)
			return res, nil
		}
		if result.ExitCode == 0 {
			return nil, fmt.Errorf("script execution failed: %w", err)
		}
	}
	if result.ExitCode != 0 {
		detail := evidence
		if detail == "" {
			detail = fmt.Sprintf("script exited with code %d", result.ExitCode)
		}
		res := j.buildResult(false, detail, debugInfo, in)
		// A nonzero exit never collects scores as a valid result (row P5): the file
		// stays a diagnostic artifact only.
		return res, nil
	}

	if evidence == "" {
		evidence = "script passed (exit code 0)"
	}
	res := j.buildResult(true, evidence, debugInfo, in)
	j.attachMetrics(res, in, false)
	return res, nil
}

// buildResult creates a judge Result from script execution outcome.
func (j *ScriptJudge) buildResult(passed bool, evidence, debugInfo string, in Input) *Result {
	if debugInfo != "" {
		evidence = evidence + " [stderr: " + debugInfo + "]"
	}

	assertion := AssertionResult{
		Text:     "script: " + j.ScriptPath,
		Passed:   passed,
		Evidence: evidence,
	}

	return NewResult([]AssertionResult{assertion}, in.TurnsExecuted, in.TurnsTotal)
}

// attachMetrics collects this invocation's structured metric result file and
// materializes one slot per planned registry entry onto the judge result.
//
// The rules it enforces: the path must resolve to a regular, non-symlink file of
// an acceptable size, bound to this invocation id, and schema-valid. A
// whole-envelope defect poisons every slot; a single bad entry poisons only its
// own slot and independently valid siblings stay AVAILABLE. A file that never
// appeared while evaluation was interrupted yields NOT_PRODUCED, and one that
// never appeared after a completed run yields a rejected payload rather than a
// fabricated zero.
func (j *ScriptJudge) attachMetrics(res *Result, in Input, prevented bool) {
	if res == nil || len(j.MetricSlots) == 0 {
		return
	}
	if j.MetricsResultPath == "" {
		// The channel is not available for this judge (a non-local runtime, or a
		// judge built without one). Report that honestly rather than implying a
		// stale file was found.
		res.Metrics = MaterializeSlots("script", j.MetricSlots, nil, ProblemChannelUnsupported, prevented)
		return
	}
	info, err := os.Lstat(j.MetricsResultPath)
	if errors.Is(err, os.ErrNotExist) && prevented {
		// Nothing was ever produced because evaluation was interrupted: the slots
		// are NOT_PRODUCED, not a rejected payload.
		res.Metrics = MaterializeSlots("script", j.MetricSlots, nil, ProblemNone, true)
		return
	}
	if err != nil {
		problem := ProblemUnboundResult
		if !os.IsNotExist(err) {
			problem = ProblemUnparseable
		}
		res.Metrics = MaterializeSlots("script", j.MetricSlots, nil, problem, prevented)
		return
	}
	if !info.Mode().IsRegular() {
		// A symlink, directory or device node is never a collected result.
		res.Metrics = MaterializeSlots("script", j.MetricSlots, nil, ProblemUnboundResult, prevented)
		return
	}
	if info.Size() > MaxMetricsPayloadBytes {
		// Bound the read before it happens: an oversized file must not be loaded
		// into memory just to be rejected afterwards.
		res.Metrics = MaterializeSlots("script", j.MetricSlots, nil, ProblemSizeLimit, prevented)
		return
	}
	raw, err := os.ReadFile(j.MetricsResultPath)
	if err != nil {
		res.Metrics = MaterializeSlots("script", j.MetricSlots, nil, ProblemUnparseable, prevented)
		return
	}
	emitted, payloadProblem := ParseMetricsPayload(raw, j.MetricsInvocationID)
	res.Metrics = MaterializeSlots("script", j.MetricSlots, emitted, payloadProblem, false)
}
