package evaluator

import (
	"context"
	"errors"

	"github.com/alibaba/skill-up/internal/config"
	"github.com/alibaba/skill-up/internal/judge"
)

func (e *defaultEvaluator) recordSkippedJudgesForCase(result *EvalResult, caseCfg *config.CaseConfig, reason string) {
	plan, err := config.ResolveJudgePlan(e.evalCfg, caseCfg)
	if err == nil {
		recordSkippedJudges(result, plan, reason)
	}
}

// finalizeJudgeMetrics applies the opt-in named-metric contract and preserves
// slots and execution facts outside grading for early returns and nil results.
func finalizeJudgeMetrics(ctx context.Context, result *EvalResult, cfg config.JudgeConfig) {
	planned := judge.SlotsFromConfig(cfg.Metrics)
	if len(planned) == 0 {
		return
	}
	stage := result.judgeStage
	if stage == "" {
		stage = judge.StageNotInvoked
	}
	env := caseMetricsEnvelope(ctx, result, cfg, planned, stage)
	result.Metrics = env
	if result.Grading != nil {
		result.Grading.Metrics = env
	}

	production := judge.MetricProduced
	if ctx.Err() != nil || (result.Grading != nil && result.Grading.Execution != nil && result.Grading.Execution.Completion == judge.CompletionInterrupted) {
		production = judge.MetricPrevented
	}
	contractError := metricContractError(env, planned, production)
	switch {
	case contractError != "":
		applyMetricError(result, contractError)
	case env.Quality == judge.QualityNotMet:
		applyMetricFailure(result)
	}
	result.Execution = deriveSingleExecution(ctx, result, env, planned, contractError, stage)
}

func caseMetricsEnvelope(ctx context.Context, result *EvalResult, cfg config.JudgeConfig, planned []judge.MetricSlot, stage judge.Stage) *judge.MetricsEnvelope {
	if result.Grading != nil && result.Grading.Metrics != nil {
		return result.Grading.Metrics
	}
	production := judge.MetricProduced
	if stage != judge.StageStarted || ctx.Err() != nil {
		production = judge.MetricPrevented
	}
	problem := judge.ProblemNone
	if cfg.Type != "script" && stage == judge.StageStarted {
		problem = judge.ProblemChannelUnsupported
	}
	return judge.MaterializeSlots(cfg.ID, planned, nil, problem, production)
}

func metricContractError(env *judge.MetricsEnvelope, planned []judge.MetricSlot, production judge.MetricProduction) string {
	byID := make(map[string]judge.MetricResult, len(env.Metrics))
	for _, metric := range env.Metrics {
		byID[metric.MetricID] = metric
		if metric.Availability == judge.Errored {
			return "named metric " + metric.MetricID + " is invalid: " + metric.Reason
		}
	}
	if production == judge.MetricPrevented {
		return ""
	}
	for _, slot := range planned {
		metric, ok := byID[slot.ID]
		if (!ok || metric.Availability != judge.Available) && (slot.Required || slot.MinScore != nil) {
			return "required named metric unavailable: " + slot.ID
		}
	}
	return ""
}

func applyMetricError(result *EvalResult, message string) {
	result.Status = judge.StatusError
	result.Error = errors.New(message)
	if result.Grading != nil {
		result.Grading.Status = judge.StatusError
		result.Grading.ErrorReason = &message
		appendMetricFailure(result.Grading, message)
	}
}

func applyMetricFailure(result *EvalResult) {
	result.Status = judge.StatusFail
	if result.Grading != nil {
		result.Grading.Status = judge.StatusFail
		appendMetricFailure(result.Grading, "one or more configured score thresholds were not met")
	}
}

func deriveSingleExecution(ctx context.Context, result *EvalResult, env *judge.MetricsEnvelope, planned []judge.MetricSlot, contractError string, stage judge.Stage) *judge.ExecutionMetadata {
	switch {
	case ctx.Err() != nil:
		stop := judge.ClassifyStop(ctx.Err(), judge.StopObservation{DeadlineSource: DeadlineSource(ctx), SignalObserved: SignalObserved(ctx)})
		return interruptedExecution(stage, stop)
	case result.Grading != nil && result.Grading.Execution != nil:
		return result.Grading.Execution
	case contractError != "":
		return &judge.ExecutionMetadata{Stage: judge.StageFinished, Completion: completionForContract(env, planned), Cause: judge.CauseContractError}
	case result.Error != nil:
		return &judge.ExecutionMetadata{Stage: stage, Completion: judge.CompletionFailed, Cause: judge.CauseExecutionError, Detail: result.Error.Error()}
	default:
		return &judge.ExecutionMetadata{Stage: judge.StageFinished, Completion: completionForContract(env, planned), Cause: judge.CauseNone}
	}
}

func appendMetricFailure(result *judge.Result, evidence string) {
	result.AssertionResults = append(result.AssertionResults, judge.AssertionResult{
		Text: "named metric contract", Passed: false, Evidence: evidence,
	})
	result.Summary.Total++
	result.Summary.Failed++
	if result.Summary.Total > 0 {
		result.Summary.PassRate = float64(result.Summary.Passed) / float64(result.Summary.Total)
	}
}
