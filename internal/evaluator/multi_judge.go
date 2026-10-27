package evaluator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alibaba/skill-up/internal/agent"
	"github.com/alibaba/skill-up/internal/config"
	"github.com/alibaba/skill-up/internal/judge"
	"github.com/alibaba/skill-up/internal/runtime"
)

// JudgeOutcome preserves one independent judge result for evaluator callers.
type JudgeOutcome = judge.Outcome

func judgePlanNeedsWorkspaceDiff(plan config.JudgePlan) bool {
	return slices.ContainsFunc(plan.Judges, judgeNeedsWorkspaceDiff)
}

func (e *defaultEvaluator) runMultipleJudges(
	ctx context.Context,
	rt runtime.Runtime,
	caseCfg *config.CaseConfig,
	configName string,
	plan config.JudgePlan,
	turnsTotal int,
	runAgent agent.Agent,
	input judge.Input,
	result *EvalResult,
) EvalResult {
	result.JudgeResults = make([]JudgeOutcome, 0, len(plan.Judges))
	var causes []error
	for i, member := range plan.Judges {
		if ctx.Err() != nil {
			stop := judge.ClassifyStop(ctx.Err(), DeadlineSource(ctx), SignalObserved(ctx))
			for _, pending := range plan.Judges[i:] {
				result.JudgeResults = append(result.JudgeResults, JudgeOutcome{
					ID: pending.ID, Type: pending.Type, Status: judge.StatusSkip,
					SkipReason: stopDetail(stop),
					Execution:  interruptedExecution(judge.StageNotInvoked, stop),
					Metrics:    preventedMetrics(pending.ID, pending.Metrics),
				})
			}
			break
		}
		started := time.Now()
		outcome := JudgeOutcome{ID: member.ID, Type: member.Type, Artifacts: "judge/" + member.ID + "/run"}
		e.prepareOutputDir(ctx, configName, caseCfg.ID, outcome.Artifacts)
		memberCtx := ctx
		memberTimeout := member.TimeoutSeconds != nil && *member.TimeoutSeconds > 0
		cancel := func() {}
		if memberTimeout {
			memberCtx, cancel = context.WithTimeout(ctx, time.Duration(*member.TimeoutSeconds)*time.Second)
		}
		memberResult := &EvalResult{SessionResult: result.SessionResult, CaseID: result.CaseID, CaseName: result.CaseName}
		graded := e.runJudgePhase(memberCtx, rt, caseCfg, configName, member, turnsTotal, runAgent, input, memberResult, outcome.Artifacts, true)
		outcome.Status, outcome.Result = graded.Status, graded.Grading
		outcome.Session, outcome.Skills = graded.JudgeSession, graded.JudgeSkills
		if graded.Grading != nil {
			outcome.Recovery = graded.Grading.Recovery
		}
		outcome.Error = judgeOutcomeError(graded)
		if graded.Status == judge.StatusError && graded.Error != nil {
			causes = append(causes, graded.Error)
		}
		// An outcome that reached a final validated state before the stop is
		// accepted and retained: a later cancellation or deadline never rewrites
		// its verdict, and its scores are not downgraded because evaluation then
		// stopped. Only members without such an outcome take the interrupt verdict.
		accepted := graded.Grading != nil && graded.Status != judge.StatusError
		stop := memberStop(ctx, memberCtx, memberTimeout)
		outcome.Metrics = gradedMetrics(member, graded, stop)
		outcome.Execution = memberExecution(graded, stop, accepted)
		if memberErr := recordMemberContextError(memberCtx, &outcome, accepted); memberErr != nil {
			causes = append(causes, memberErr)
		}
		cancel()
		outcome.DurationMs = time.Since(started).Milliseconds()
		result.JudgeResults = append(result.JudgeResults, outcome)
	}
	result.Grading, result.Status = judge.DefaultOutcomeAggregator().Aggregate(result.JudgeResults, result.Turns, turnsTotal)
	result.GroupCompletion, result.GroupCause = foldGroupState(result.JudgeResults)
	if result.Status == judge.StatusError {
		result.Error = multiJudgeError(ctx, result.JudgeResults, causes)
	}
	result.Configuration = configName
	return *result
}

func recordMemberContextError(ctx context.Context, outcome *JudgeOutcome, accepted bool) error {
	err := ctx.Err()
	if err == nil {
		return nil
	}
	// An accepted outcome keeps its own verdict; the stop is still recorded so the
	// timeout or cancellation stays visible in the execution metadata.
	if !accepted && outcome.Status != judge.StatusError {
		outcome.Status = judge.StatusError
		outcome.Error = err.Error()
	}
	return err
}

// memberStop resolves why a member stopped. A member-level deadline is only
// attributed to judge.timeout_seconds when the parent context had not itself
// expired, so a case deadline firing through the member is never mislabeled.
func memberStop(parentCtx, memberCtx context.Context, memberTimeout bool) judge.Stop {
	if memberCtx.Err() == nil {
		return judge.Stop{Cause: judge.CauseNone}
	}
	source := DeadlineSource(parentCtx)
	if memberTimeout && source == "" && parentCtx.Err() == nil && errors.Is(memberCtx.Err(), context.DeadlineExceeded) {
		source = "judge.timeout_seconds"
	}
	return judge.ClassifyStop(memberCtx.Err(), source, SignalObserved(parentCtx))
}

// stopDetail renders a human-readable stop note. The machine-readable cause and
// source live in ExecutionMetadata and are never re-derived from this text.
func stopDetail(stop judge.Stop) string {
	switch stop.Cause {
	case judge.CauseDeadlineExpiry:
		return "case deadline expired"
	case judge.CauseCancellation:
		return "cancelled before invocation"
	case judge.CauseExpectGate:
		return "gate_failed"
	case judge.CauseNone:
		return ""
	default:
		return stop.Detail
	}
}

func interruptedExecution(stage judge.Stage, stop judge.Stop) *judge.ExecutionMetadata {
	return &judge.ExecutionMetadata{
		Stage:          stage,
		Completion:     judge.CompletionInterrupted,
		Cause:          stop.Cause,
		DeadlineSource: stop.DeadlineSource,
		CancelSource:   stop.CancelSource,
		Detail:         stop.Detail,
	}
}

// preventedMetrics materializes every planned slot as NOT_PRODUCED for a member
// whose evaluation never reached a final outcome. The planned denominator is the
// registry, so a member that never started still reports each slot.
func preventedMetrics(judgeID string, registry []config.MetricConfig) *judge.MetricsEnvelope {
	slots := judge.SlotsFromConfig(registry)
	if len(slots) == 0 {
		return nil
	}
	return judge.MaterializeSlots(judgeID, slots, nil, judge.ProblemNone, true)
}

// gradedMetrics builds the envelope from what a judge produced. A stop that
// happened after a usable score never discards that score; slots that were never
// reached stay NOT_PRODUCED rather than becoming zero.
func gradedMetrics(member config.JudgeConfig, graded EvalResult, stop judge.Stop) *judge.MetricsEnvelope {
	slots := judge.SlotsFromConfig(member.Metrics)
	if len(slots) == 0 {
		return nil
	}
	if graded.Grading != nil && graded.Grading.Metrics != nil {
		return graded.Grading.Metrics
	}
	prevented := stop.Cause != judge.CauseNone && graded.Grading == nil
	return judge.MaterializeSlots(member.ID, slots, nil, judge.ProblemNone, prevented)
}

// memberExecution describes one member's execution facts. Accepted results report
// their own completion; a stopped member without a final outcome is INTERRUPTED
// whether or not it had started, with the stage recording which of the two it was.
func memberExecution(graded EvalResult, stop judge.Stop, accepted bool) *judge.ExecutionMetadata {
	if accepted {
		completion := judge.CompletionFinished
		if graded.Grading.Metrics != nil {
			completion = completionForContract(graded.Grading.Metrics)
		}
		meta := &judge.ExecutionMetadata{
			Stage:      judge.StageFinished,
			Completion: completion,
			Cause:      judge.CauseNone,
		}
		if stop.Cause != judge.CauseNone {
			meta.Cause, meta.DeadlineSource, meta.CancelSource, meta.Detail = stop.Cause, stop.DeadlineSource, stop.CancelSource, stop.Detail
			if stop.Cause == judge.CauseDeadlineExpiry {
				meta.Timeout = &judge.TimeoutMetadata{Occurred: true, Source: stop.DeadlineSource, Recovered: graded.Grading.Recovery != nil}
			}
		}
		return meta
	}
	stage := judge.StageStarted
	if graded.Grading == nil && graded.Status == judge.StatusSkip {
		stage = judge.StageNotInvoked
	}
	execution := interruptedExecution(stage, stop)
	if execution.Cause == judge.CauseNone {
		execution.Cause = judge.CauseExecutionError
		if graded.Error != nil {
			execution.Detail = graded.Error.Error()
		}
		if graded.Grading != nil {
			execution.Completion = judge.CompletionFailed
		}
	}
	return execution
}

// completionForContract maps the materialized slots onto the completion states:
// an errored slot failed the contract, an unfulfilled required slot or an
// unevaluable threshold leaves the outcome incomplete.
func completionForContract(env *judge.MetricsEnvelope) judge.Completion {
	requiredMissing := false
	for _, metric := range env.Metrics {
		if metric.Availability == judge.Errored {
			return judge.CompletionFailed
		}
	}
	for _, metric := range env.Metrics {
		if metric.Availability != judge.Available {
			requiredMissing = true
		}
	}
	if requiredMissing {
		return judge.CompletionIncomplete
	}
	return judge.CompletionFinished
}

func multiJudgeError(ctx context.Context, outcomes []JudgeOutcome, causes []error) error {
	if ctx.Err() != nil {
		causes = append(causes, ctx.Err())
	}
	summary := errors.New(summarizeJudgeErrors(outcomes))
	if len(causes) == 0 {
		return summary
	}
	return fmt.Errorf("%w: %w", summary, errors.Join(causes...))
}

func judgeOutcomeError(graded EvalResult) string {
	if graded.Error != nil {
		return graded.Error.Error()
	}
	if graded.Grading != nil && graded.Grading.ErrorReason != nil {
		return *graded.Grading.ErrorReason
	}
	return ""
}

func summarizeJudgeErrors(outcomes []JudgeOutcome) string {
	var details []string
	for _, outcome := range outcomes {
		if outcome.Status == judge.StatusError || outcome.Status == judge.StatusSkip {
			detail := outcome.Error
			if detail == "" {
				detail = outcome.SkipReason
			}
			details = append(details, outcome.ID+": "+detail)
		}
	}
	return "judges failed to complete: " + strings.Join(details, "; ")
}

// recordSkippedJudges materializes every planned member as skipped. Each member
// keeps execution metadata and its registry-derived metric slots even though no
// judge ran, so a gate or execution failure never loses that information. The
// reason strings are unchanged for compatibility; the machine-readable cause is
// the new source of truth.
func recordSkippedJudges(result *EvalResult, plan config.JudgePlan, reason string) {
	if !plan.Multi {
		return
	}
	cause := causeForReason(reason)
	for _, member := range plan.Judges {
		result.JudgeResults = append(result.JudgeResults, JudgeOutcome{
			ID: member.ID, Type: member.Type, Status: judge.StatusSkip, SkipReason: reason,
			Execution: interruptedExecution(judge.StageNotInvoked, judge.Stop{Cause: cause, Detail: reason}),
			Metrics:   preventedMetrics(member.ID, member.Metrics),
		})
	}
}

func causeForReason(reason string) judge.Cause {
	switch reason {
	case "gate_failed":
		return judge.CauseExpectGate
	case "session_resumption_unsupported", "invalid_judge_config":
		return judge.CauseContractError
	default:
		return judge.CauseExecutionError
	}
}

// foldGroupState derives plan-level state from the members: completion folds over
// member completions by the fixed rank, and the cause is the first reason that
// actually ended the plan. A per-member judge_declined is a scoring reason rather
// than a plan terminator, so it does not become the group cause. Members without
// execution metadata (legacy judges with no metric registry) contribute nothing,
// and an empty result leaves both values unset so the artifact omits them.
func foldGroupState(outcomes []JudgeOutcome) (judge.Completion, judge.Cause) {
	completions := make([]judge.Completion, 0, len(outcomes))
	cause := judge.CauseNone
	for _, outcome := range outcomes {
		if outcome.Execution == nil {
			continue
		}
		completions = append(completions, outcome.Execution.Completion)
		if cause == judge.CauseNone && outcome.Execution.Cause != judge.CauseNone &&
			outcome.Execution.Cause != judge.CauseJudgeDeclined {
			cause = outcome.Execution.Cause
		}
	}
	if len(completions) == 0 {
		return "", judge.CauseNone
	}
	completion, err := judge.FoldGroup(completions)
	if err != nil {
		return "", cause
	}
	return completion, cause
}
