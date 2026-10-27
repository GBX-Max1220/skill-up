package judge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/alibaba/skill-up/internal/config"
)

func f(v float64) *float64 { return &v }

func twoSlots() []MetricSlot {
	return []MetricSlot{{ID: "correctness", Required: true}, {ID: "completeness"}}
}

func TestFoldGroup(t *testing.T) {
	cases := []struct {
		name    string
		members []Completion
		want    Completion
		wantErr bool
	}{
		{"completed plus pending cancelled", []Completion{CompletionFinished, CompletionInterrupted}, CompletionInterrupted, false},
		{"failed plus pending cancelled keeps failed", []Completion{CompletionFailed, CompletionInterrupted}, CompletionFailed, false},
		{"incomplete plus pending cancelled keeps incomplete", []Completion{CompletionIncomplete, CompletionInterrupted}, CompletionIncomplete, false},
		{"failed outranks incomplete", []Completion{CompletionIncomplete, CompletionFailed}, CompletionFailed, false},
		{"all completed", []Completion{CompletionFinished, CompletionFinished}, CompletionFinished, false},
		{"never started is still interrupted not a new state", []Completion{CompletionInterrupted, CompletionInterrupted}, CompletionInterrupted, false},
		{"single member", []Completion{CompletionIncomplete}, CompletionIncomplete, false},
		{"empty group is refused", []Completion{}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FoldGroup(tc.members)
			if tc.wantErr {
				if !errors.Is(err, ErrEmptyGroup) {
					t.Fatalf("want ErrEmptyGroup, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if got != tc.want {
				t.Fatalf("fold = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDeriveQuality(t *testing.T) {
	cases := []struct {
		name    string
		planned []MetricSlot
		got     []MetricResult
		want    Quality
	}{
		{
			"thresholds configured and all satisfied",
			[]MetricSlot{{ID: "a", MinScore: f(0.7)}, {ID: "b", MinScore: f(0.5)}},
			[]MetricResult{{MetricID: "a", Availability: Available, Value: f(0.7)}, {MetricID: "b", Availability: Available, Value: f(0.9)}},
			QualityMet,
		},
		{
			"threshold boundary is inclusive",
			[]MetricSlot{{ID: "a", MinScore: f(0.75)}},
			[]MetricResult{{MetricID: "a", Availability: Available, Value: f(0.75)}},
			QualityMet,
		},
		{
			"thresholds configured and one below",
			[]MetricSlot{{ID: "a", MinScore: f(0.7)}, {ID: "b", MinScore: f(0.7)}},
			[]MetricResult{{MetricID: "a", Availability: Available, Value: f(0.9)}, {MetricID: "b", Availability: Available, Value: f(0.2)}},
			QualityNotMet,
		},
		{
			"thresholded value unavailable yields UNKNOWN",
			[]MetricSlot{{ID: "a", MinScore: f(0.7)}, {ID: "b", MinScore: f(0.7)}},
			[]MetricResult{{MetricID: "a", Availability: Available, Value: f(0.9)}, {MetricID: "b", Availability: Declined, Reason: "declined_by_judge"}},
			QualityUnknown,
		},
		{
			"cancellation with thresholds configured yields UNKNOWN",
			[]MetricSlot{{ID: "a", MinScore: f(0.7)}},
			[]MetricResult{{MetricID: "a", Availability: NotProduced, Reason: "evaluation_prevented"}},
			QualityUnknown,
		},
		{
			"gate short-circuit with thresholds configured yields UNKNOWN",
			[]MetricSlot{{ID: "a", MinScore: f(0.7)}, {ID: "b", MinScore: f(0.7)}},
			[]MetricResult{{MetricID: "a", Availability: NotProduced}, {MetricID: "b", Availability: NotProduced}},
			QualityUnknown,
		},
		{
			"valid scores and no thresholds is NOT_CONFIGURED",
			twoSlots(),
			[]MetricResult{{MetricID: "correctness", Availability: Available, Value: f(0.8)}, {MetricID: "completeness", Availability: Available, Value: f(0.6)}},
			QualityNotConfigured,
		},
		{
			"an explicit zero score is a valid score",
			twoSlots(),
			[]MetricResult{{MetricID: "correctness", Availability: Available, Value: f(0)}},
			QualityNotConfigured,
		},
		{
			"no thresholds and no valid scores is NOT_ASSESSED",
			twoSlots(),
			[]MetricResult{{MetricID: "correctness", Availability: Missing}, {MetricID: "completeness", Availability: Declined}},
			QualityNotAssessed,
		},
		{
			"cancelled slots without thresholds are NOT_ASSESSED",
			twoSlots(),
			[]MetricResult{{MetricID: "correctness", Availability: NotProduced}, {MetricID: "completeness", Availability: NotProduced}},
			QualityNotAssessed,
		},
		{
			"an errored value is not a valid score",
			twoSlots(),
			[]MetricResult{{MetricID: "correctness", Availability: Errored, Reason: "invalid_value"}},
			QualityNotAssessed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveQuality(tc.planned, tc.got); got != tc.want {
				t.Fatalf("quality = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestValidScore(t *testing.T) {
	cases := []struct {
		name string
		val  *float64
		want bool
	}{
		{"nil", nil, false},
		{"zero is valid", f(0), true},
		{"one is valid", f(1), true},
		{"negative", f(-0.01), false},
		{"above one", f(1.4), false},
		{"NaN is never clamped", f(math.NaN()), false},
		{"positive infinity", f(math.Inf(1)), false},
		{"negative infinity", f(math.Inf(-1)), false},
		{"mid range", f(0.75), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidScore(tc.val); got != tc.want {
				t.Fatalf("ValidScore = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMaterializeSlotsAvailability(t *testing.T) {
	cases := []struct {
		name        string
		emitted     []EmittedMetric
		prevented   bool
		want        map[string]Availability
		wantDiags   []string
		wantPlanned int
	}{
		{
			name:        "valid two-metric output",
			emitted:     []EmittedMetric{{MetricID: "correctness", Availability: "AVAILABLE", Value: f(0.8)}, {MetricID: "completeness", Availability: "AVAILABLE", Value: f(0.5)}},
			want:        map[string]Availability{"correctness": Available, "completeness": Available},
			wantPlanned: 2,
		},
		{
			name:        "optional omission is MISSING and required sibling survives",
			emitted:     []EmittedMetric{{MetricID: "correctness", Availability: "AVAILABLE", Value: f(0.9)}},
			want:        map[string]Availability{"correctness": Available, "completeness": Missing},
			wantPlanned: 2,
		},
		{
			name:        "unknown id is a diagnostic and never a new slot",
			emitted:     []EmittedMetric{{MetricID: "correctness", Availability: "AVAILABLE", Value: f(0.9)}, {MetricID: "invented", Availability: "AVAILABLE", Value: f(0.1)}},
			want:        map[string]Availability{"correctness": Available, "completeness": Missing},
			wantDiags:   []string{"unknown_metric_id: invented"},
			wantPlanned: 2,
		},
		{
			name:        "duplicate id invalidates that planned slot once and keeps no other slot",
			emitted:     []EmittedMetric{{MetricID: "correctness", Availability: "AVAILABLE", Value: f(0.9)}, {MetricID: "correctness", Availability: "AVAILABLE", Value: f(0.2)}, {MetricID: "completeness", Availability: "AVAILABLE", Value: f(0.4)}},
			want:        map[string]Availability{"correctness": Errored, "completeness": Available},
			wantDiags:   []string{"duplicate_metric_id: correctness"},
			wantPlanned: 2,
		},
		{
			name:        "out of range value poisons only its own slot",
			emitted:     []EmittedMetric{{MetricID: "correctness", Availability: "AVAILABLE", Value: f(1.4)}, {MetricID: "completeness", Availability: "AVAILABLE", Value: f(0.4)}},
			want:        map[string]Availability{"correctness": Errored, "completeness": Available},
			wantPlanned: 2,
		},
		{
			name:        "non-finite value poisons only its own slot",
			emitted:     []EmittedMetric{{MetricID: "correctness", Availability: "AVAILABLE", Value: f(math.NaN())}, {MetricID: "completeness", Availability: "AVAILABLE", Value: f(0.4)}},
			want:        map[string]Availability{"correctness": Errored, "completeness": Available},
			wantPlanned: 2,
		},
		{
			name:        "judge-declared SKIP keeps its own reason and is never zero",
			emitted:     []EmittedMetric{{MetricID: "correctness", Availability: "AVAILABLE", Value: f(0.8)}, {MetricID: "completeness", Availability: "SKIP", Reason: "insufficient evidence"}},
			want:        map[string]Availability{"correctness": Available, "completeness": Declined},
			wantPlanned: 2,
		},
		{
			name:        "cancelled before or during invocation produces NOT_PRODUCED, never SKIP",
			prevented:   true,
			want:        map[string]Availability{"correctness": NotProduced, "completeness": NotProduced},
			wantPlanned: 2,
		},
		{
			name:        "a score obtained before the stop is not downgraded to NOT_PRODUCED",
			prevented:   true,
			emitted:     []EmittedMetric{{MetricID: "correctness", Availability: "AVAILABLE", Value: f(0.7)}},
			want:        map[string]Availability{"correctness": Available, "completeness": NotProduced},
			wantPlanned: 2,
		},
		{
			name:        "a SKIP or MISSING claim from a stopped evaluation is NOT_PRODUCED, not a decline",
			prevented:   true,
			emitted:     []EmittedMetric{{MetricID: "correctness", Availability: "SKIP", Reason: "ran out of time"}},
			want:        map[string]Availability{"correctness": NotProduced, "completeness": NotProduced},
			wantPlanned: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := MaterializeSlots("j1", twoSlots(), tc.emitted, ProblemNone, tc.prevented)
			if len(env.Metrics) != tc.wantPlanned {
				t.Fatalf("materialized %d slots, planned %d: denominator must not move", len(env.Metrics), tc.wantPlanned)
			}
			if env.Coverage.Planned != tc.wantPlanned {
				t.Fatalf("coverage planned = %d, want %d", env.Coverage.Planned, tc.wantPlanned)
			}
			got := map[string]Availability{}
			for _, m := range env.Metrics {
				got[m.MetricID] = m.Availability
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("availability = %v, want %v", got, tc.want)
			}
			if !reflect.DeepEqual(env.Diagnostics, tc.wantDiags) && len(tc.wantDiags) > 0 {
				t.Fatalf("diagnostics = %v, want %v", env.Diagnostics, tc.wantDiags)
			}
			for _, m := range env.Metrics {
				if m.Availability != Available && m.Value != nil {
					t.Fatalf("slot %s with availability %s carries a value %v; unusable states must stay valueless", m.MetricID, m.Availability, *m.Value)
				}
			}
		})
	}
}

func TestMaterializeSlotsPayloadProblems(t *testing.T) {
	problems := []PayloadProblem{ProblemUnparseable, ProblemTopLevelShape, ProblemDuplicateJSONKey,
		ProblemUnknownTopField, ProblemSchemaVersion, ProblemUnboundResult, ProblemSizeLimit}
	for _, p := range problems {
		t.Run(string(p), func(t *testing.T) {
			// A well-formed pair of scores must still be discarded, because the
			// envelope as a whole cannot be trusted.
			emitted := []EmittedMetric{
				{MetricID: "correctness", Availability: "AVAILABLE", Value: f(0.8)},
				{MetricID: "completeness", Availability: "AVAILABLE", Value: f(0.6)},
			}
			env := MaterializeSlots("j1", twoSlots(), emitted, p, false)
			for _, m := range env.Metrics {
				if m.Availability != Errored || m.Reason != string(p) || m.Value != nil {
					t.Fatalf("slot %s = %+v, want ERROR/%s with no value", m.MetricID, m, p)
				}
			}
			if env.Coverage.Planned != 2 || env.Coverage.Errored != 2 {
				t.Fatalf("coverage = %+v, want planned 2 and errored 2", env.Coverage)
			}
			if env.Quality != QualityNotAssessed {
				t.Fatalf("quality = %s, want NOT_ASSESSED", env.Quality)
			}
		})
	}
}

func TestSlotLevelDefectKeepsSiblingsUsable(t *testing.T) {
	planned := []MetricSlot{{ID: "correctness", Required: true}, {ID: "completeness"}, {ID: "instruction_following"}}
	emitted := []EmittedMetric{
		{MetricID: "correctness", Availability: "AVAILABLE", Value: f(0.9)},
		{MetricID: "completeness", Availability: "AVAILABLE", Value: f(2.5)}, // invalid
		{MetricID: "instruction_following", Availability: "AVAILABLE", Value: f(0.5)},
	}
	env := MaterializeSlots("j1", planned, emitted, ProblemNone, false)
	if env.Metrics[0].Availability != Available || env.Metrics[2].Availability != Available {
		t.Fatalf("valid siblings must stay AVAILABLE: %+v", env.Metrics)
	}
	if env.Metrics[1].Availability != Errored || env.Metrics[1].Reason != "invalid_value" {
		t.Fatalf("defective slot = %+v", env.Metrics[1])
	}
	if env.Coverage.Available != 2 || env.Coverage.Errored != 1 || env.Coverage.Planned != 3 {
		t.Fatalf("coverage = %+v, want 2 available of 3 planned", env.Coverage)
	}
	if *env.Coverage.Ratio != 2.0/3.0 {
		t.Fatalf("ratio = %v, want 2/3", *env.Coverage.Ratio)
	}
	if env.Quality != QualityNotConfigured {
		t.Fatalf("quality = %s, want NOT_CONFIGURED", env.Quality)
	}
}

func TestEvidenceRequirementIsPerSlot(t *testing.T) {
	planned := []MetricSlot{{ID: "correctness", EvidenceRequired: true}, {ID: "completeness"}}
	env := MaterializeSlots("j1", planned, []EmittedMetric{
		{MetricID: "correctness", Availability: "AVAILABLE", Value: f(0.8)},
		{MetricID: "completeness", Availability: "AVAILABLE", Value: f(0.8)},
	}, ProblemNone, false)
	if env.Metrics[0].Availability != Errored || env.Metrics[0].Reason != "missing_evidence" {
		t.Fatalf("evidence-less slot = %+v", env.Metrics[0])
	}
	if env.Metrics[1].Availability != Available {
		t.Fatalf("sibling without an evidence requirement must stay usable: %+v", env.Metrics[1])
	}
}

func TestCoverageWithoutPlannedSlotsStaysNull(t *testing.T) {
	env := MaterializeSlots("j1", nil, nil, ProblemNone, false)
	if env.Coverage.Planned != 0 || env.Coverage.Ratio != nil {
		t.Fatalf("coverage = %+v, want planned 0 and null ratio", env.Coverage)
	}
	if env.Quality != QualityNotAssessed {
		t.Fatalf("quality = %s, want NOT_ASSESSED", env.Quality)
	}
}

func TestSlotsFromConfigKeepsThePlannedDenominator(t *testing.T) {
	registry := []config.MetricConfig{
		{ID: "correctness", Required: true, EvidenceRequired: true},
		{ID: "completeness", MinScore: f(0.75)},
	}
	slots := SlotsFromConfig(registry)
	if len(slots) != 2 {
		t.Fatalf("slots = %+v, want 2", slots)
	}
	if !slots[0].Required || !slots[0].EvidenceRequired || slots[0].MinScore != nil {
		t.Fatalf("slot 0 = %+v", slots[0])
	}
	if slots[1].MinScore == nil || *slots[1].MinScore != 0.75 {
		t.Fatalf("slot 1 = %+v", slots[1])
	}
	if SlotsFromConfig(nil) != nil {
		t.Fatal("a judge without a registry must keep no slots")
	}
	// The registry, not a returned payload, sets the denominator.
	env := MaterializeSlots("j1", slots, []EmittedMetric{{MetricID: "correctness", Availability: "AVAILABLE", Value: f(0.9)}}, ProblemNone, false)
	if env.Coverage.Planned != 2 {
		t.Fatalf("planned = %d, want the registry count of 2", env.Coverage.Planned)
	}
}

func TestClassifyStop(t *testing.T) {
	cases := []struct {
		name           string
		err            error
		deadlineSource string
		signal         bool
		want           Stop
	}{
		{"no error means no cause", nil, "", false, Stop{Cause: CauseNone}},
		{
			"deadline with a known knob names it",
			context.DeadlineExceeded, "judge.timeout_seconds", true,
			Stop{Cause: CauseDeadlineExpiry, DeadlineSource: "judge.timeout_seconds", Detail: "context deadline exceeded"},
		},
		{
			"deadline without a known origin stays unknown",
			context.DeadlineExceeded, "", false,
			Stop{Cause: CauseDeadlineExpiry, DeadlineSource: CancelUnspecified, Detail: "context deadline exceeded"},
		},
		{
			"cancellation observed as a process signal is attributed",
			context.Canceled, "", true,
			Stop{Cause: CauseCancellation, CancelSource: CancelOSSignal, Detail: "context canceled"},
		},
		{
			"a bare cancellation is never claimed as user-initiated",
			context.Canceled, "", false,
			Stop{Cause: CauseCancellation, CancelSource: CancelUnspecified, Detail: "context canceled"},
		},
		{
			"a wrapped cancellation is still a cancellation",
			fmt.Errorf("judge session failed: %w", context.Canceled), "", false,
			Stop{Cause: CauseCancellation, CancelSource: CancelUnspecified, Detail: "judge session failed: context canceled"},
		},
		{
			"an unrelated error is an execution error",
			errors.New("transport reset"), "", false,
			Stop{Cause: CauseExecutionError, Detail: "transport reset"},
		},
		{
			"a deadline wins over a signal when both fired",
			context.DeadlineExceeded, "case.constraints.timeout_seconds", true,
			Stop{Cause: CauseDeadlineExpiry, DeadlineSource: "case.constraints.timeout_seconds", Detail: "context deadline exceeded"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyStop(tc.err, tc.deadlineSource, tc.signal)
			if got != tc.want {
				t.Fatalf("ClassifyStop = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCompareScoringDefinitions(t *testing.T) {
	base := ScoringDefinition{JudgeID: "j1", MetricID: "correctness", RubricHash: "sha:rubric", ScaleHash: "sha:scale",
		Provider: "openai", Model: "gpt-x", ModelRevision: "2026-05-01"}
	cases := []struct {
		name         string
		other        ScoringDefinition
		wantSame     bool
		wantReasons  []string
		wantAdvisory []string
	}{
		{
			name:     "identical definition is comparable with no advisory",
			other:    base,
			wantSame: true,
		},
		{
			name:         "different provider and model with the same scoring definition still compares",
			other:        withIdentity(base, "anthropic", "claude-y", "2026-06-01"),
			wantSame:     true,
			wantAdvisory: []string{"provider_differs", "model_alias_differs", "model_revision_differs"},
		},
		{
			name:         "unknown revision is advisory only",
			other:        withIdentity(base, "openai", "gpt-x", ""),
			wantSame:     true,
			wantAdvisory: []string{"model_revision_unknown"},
		},
		{
			name:         "known but differing revision is advisory only",
			other:        withIdentity(base, "openai", "gpt-x", "2026-01-01"),
			wantSame:     true,
			wantAdvisory: []string{"model_revision_differs"},
		},
		{
			name:        "rubric difference blocks",
			other:       withRubric(base, "sha:other"),
			wantSame:    false,
			wantReasons: []string{"rubric_mismatch"},
		},
		{
			name:        "scoring scale difference blocks",
			other:       withScale(base, "sha:other-scale"),
			wantSame:    false,
			wantReasons: []string{"scoring_scale_mismatch"},
		},
		{
			name:        "metric id difference blocks",
			other:       withMetric(base, "completeness"),
			wantSame:    false,
			wantReasons: []string{"metric_id_mismatch"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CompareScoringDefinitions(base, tc.other)
			if got.Comparable != tc.wantSame {
				t.Fatalf("comparable = %v, want %v (reasons %v)", got.Comparable, tc.wantSame, got.Reasons)
			}
			if !reflect.DeepEqual(got.Reasons, tc.wantReasons) && len(tc.wantReasons) > 0 {
				t.Fatalf("reasons = %v, want %v", got.Reasons, tc.wantReasons)
			}
			if len(tc.wantReasons) == 0 && len(got.Reasons) != 0 {
				t.Fatalf("unexpected blocking reasons %v", got.Reasons)
			}
			if !reflect.DeepEqual(got.Advisories, tc.wantAdvisory) && len(tc.wantAdvisory) > 0 {
				t.Fatalf("advisories = %v, want %v", got.Advisories, tc.wantAdvisory)
			}
			if len(tc.wantAdvisory) == 0 && len(got.Advisories) != 0 {
				t.Fatalf("unexpected advisories %v", got.Advisories)
			}
		})
	}
}

func withIdentity(d ScoringDefinition, provider, model, rev string) ScoringDefinition {
	d.Provider, d.Model, d.ModelRevision = provider, model, rev
	return d
}

func withRubric(d ScoringDefinition, h string) ScoringDefinition  { d.RubricHash = h; return d }
func withScale(d ScoringDefinition, h string) ScoringDefinition   { d.ScaleHash = h; return d }
func withMetric(d ScoringDefinition, id string) ScoringDefinition { d.MetricID = id; return d }
