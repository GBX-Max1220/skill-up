package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alibaba/skill-up/internal/evaluator"
	"github.com/alibaba/skill-up/internal/judge"
	"github.com/alibaba/skill-up/internal/report"
)

func TestWriteCaseArtifactsPersistsSingleJudgeMetricsAndExecution(t *testing.T) {
	ws, err := report.NewIterationWorkspace(t.TempDir(), "skill", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.EnsureDirs([]string{"single"}); err != nil {
		t.Fatal(err)
	}
	score := 0.0
	result := &evaluator.EvalResult{
		CaseID: "single", CaseName: "single judge metrics", Status: judge.StatusPass,
		Metrics: judge.MaterializeSlots("configured-script-id", []judge.MetricSlot{{ID: "correctness", Required: true}}, []judge.EmittedMetric{{
			MetricID: "correctness", Availability: "AVAILABLE", Value: &score,
		}}, judge.ProblemNone, judge.MetricProduced),
		Execution: &judge.ExecutionMetadata{Stage: judge.StageFinished, Completion: judge.CompletionFinished, Cause: judge.CauseNone},
	}
	if err := (&Runner{}).writeCaseArtifacts(ws, result, "with_skill"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(ws.ConfigDir("single", "with_skill"), "evaluation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved report.GroupedEvaluation
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.JudgeResults) != 0 || saved.Execution == nil || saved.Execution.Completion != judge.CompletionFinished {
		t.Fatalf("single execution metadata lost: %+v", saved)
	}
	if saved.Metrics == nil || saved.Metrics.JudgeID != "configured-script-id" || saved.Metrics.Coverage.Available != 1 {
		t.Fatalf("single metrics lost: %+v", saved.Metrics)
	}
	if saved.Metrics.Metrics[0].Value == nil || *saved.Metrics.Metrics[0].Value != 0 {
		t.Fatalf("explicit zero lost: %+v", saved.Metrics.Metrics[0])
	}
}
