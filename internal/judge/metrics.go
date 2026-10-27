package judge

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/alibaba/skill-up/internal/config"
)

// Named-metrics scoring contract for #317, first slice (validated scoring and
// persistence). Semantics follow the maintainer decision in
// https://github.com/alibaba/skill-up/issues/317#issuecomment-6437995472 .
//
// Report integration and cross-run comparison rendering land in later bounded
// PRs.

// Completion describes how far a member's evaluation got. There is deliberately
// no NOT_RUN state: a member that never started and a member stopped mid-flight
// are both INTERRUPTED, and ExecutionStage is what distinguishes them.
type Completion string

const (
	// CompletionFinished means a final validated outcome was obtained and the
	// declared scoring requirements were satisfied. It is not a quality claim.
	CompletionFinished Completion = "COMPLETED"
	// CompletionIncomplete means a final structurally valid outcome was obtained
	// but a required metric or configured threshold could not be evaluated
	// because its score was unavailable.
	CompletionIncomplete Completion = "INCOMPLETE"
	// CompletionFailed means an execution or scoring-contract error prevented a
	// trustworthy outcome, or an explicit metric error occurred. Independently
	// validated sibling scores may still be usable.
	CompletionFailed Completion = "FAILED"
	// CompletionInterrupted means evaluation was prevented or stopped before a
	// final scoring outcome was obtained.
	CompletionInterrupted Completion = "INTERRUPTED"
)

// Cause records why evaluation ended, always separate from Completion so a cause
// never has to be inferred from a verdict.
type Cause string

const (
	CauseNone           Cause = "none"
	CauseCancellation   Cause = "cancellation"
	CauseDeadlineExpiry Cause = "deadline_expiry"
	CauseExpectGate     Cause = "expect_gate"
	CauseProcessExit    Cause = "process_exit"
	CauseExecutionError Cause = "execution_error"
	CauseContractError  Cause = "contract_error"
	CauseJudgeDeclined  Cause = "judge_declined"
)

// Stage is the program-recorded execution stage. INTERRUPTED members that
// actually ran are distinguished from never-started ones here, and no start
// timestamp is invented when one was not recorded.
type Stage string

const (
	StageNotInvoked Stage = "NOT_INVOKED"
	StageStarted    Stage = "STARTED"
	StageFinished   Stage = "COMPLETED"
)

// Availability describes one planned metric slot.
type Availability string

const (
	Available   Availability = "AVAILABLE"
	Missing     Availability = "MISSING"
	Errored     Availability = "ERROR"
	Declined    Availability = "SKIP"
	NotProduced Availability = "NOT_PRODUCED"
)

// Quality is the threshold-decision state. It is always derived, never assigned.
type Quality string

const (
	QualityMet           Quality = "MET"
	QualityNotMet        Quality = "NOT_MET"
	QualityUnknown       Quality = "UNKNOWN"
	QualityNotConfigured Quality = "NOT_CONFIGURED"
	QualityNotAssessed   Quality = "NOT_ASSESSED"
)

// Cancellation and deadline sources are recorded only when actually observed.
// A bare context.Canceled does not establish user intent, so it stays unknown.
const (
	CancelOSSignal      = "os_signal"
	CancelParentContext = "parent_context"
	CancelChildScope    = "child_scope"
	CancelUnspecified   = "unknown"
)

// ErrEmptyGroup guards group folding against inventing a state.
var ErrEmptyGroup = errors.New("judge: cannot fold completion over an empty member group")

// Stop classifies why evaluation ended. The cause never comes from an error
// string: only the observed context error, the deadline knob that was configured
// and whether a process signal was actually seen.
type Stop struct {
	Cause          Cause
	DeadlineSource string
	CancelSource   string
	Detail         string
}

// ClassifyStop turns an observed context error into a cause plus its source.
// deadlineSource names the configuration knob when a deadline was configured and
// empty means the deadline's origin is unknown; signalObserved is true only when
// the process actually saw an interrupt, because a bare context.Canceled does not
// establish user-initiated cancellation.
func ClassifyStop(ctxErr error, deadlineSource string, signalObserved bool) Stop {
	switch {
	case ctxErr == nil:
		return Stop{Cause: CauseNone}
	case errors.Is(ctxErr, context.DeadlineExceeded):
		source := deadlineSource
		if source == "" {
			source = CancelUnspecified
		}
		return Stop{Cause: CauseDeadlineExpiry, DeadlineSource: source, Detail: ctxErr.Error()}
	case errors.Is(ctxErr, context.Canceled):
		source := CancelUnspecified
		if signalObserved {
			source = CancelOSSignal
		}
		return Stop{Cause: CauseCancellation, CancelSource: source, Detail: ctxErr.Error()}
	default:
		return Stop{Cause: CauseExecutionError, Detail: ctxErr.Error()}
	}
}

// MetricSlot is one planned registry slot. The denominator is fixed by
// configuration before execution, never by what a judge happened to return.
type MetricSlot struct {
	ID               string
	Required         bool
	MinScore         *float64
	EvidenceRequired bool
}

// MetricResult is the materialized state of one planned slot.
type MetricResult struct {
	MetricID     string       `json:"metric_id"`
	Availability Availability `json:"availability"`
	Value        *float64     `json:"value"`
	Reason       string       `json:"reason,omitempty"`
	Evidence     []string     `json:"evidence,omitempty"`
}

// Coverage counts one bucket per availability over the planned total.
type Coverage struct {
	Planned     int      `json:"planned"`
	Available   int      `json:"available"`
	Missing     int      `json:"missing"`
	Errored     int      `json:"error"`
	Skipped     int      `json:"skip"`
	NotProduced int      `json:"not_produced"`
	Ratio       *float64 `json:"ratio"`
}

// MetricsEnvelope is the additive named-metrics payload. Legacy results omit it.
type MetricsEnvelope struct {
	JudgeID     string         `json:"judge_id"`
	Metrics     []MetricResult `json:"metrics"`
	Quality     Quality        `json:"metric_quality_state"`
	Coverage    Coverage       `json:"coverage"`
	Diagnostics []string       `json:"diagnostics,omitempty"`
}

// ExecutionMetadata preserves how far evaluation got and why it stopped. It is
// carried even when no scoring Result exists at all.
type ExecutionMetadata struct {
	Stage          Stage            `json:"execution_stage"`
	Completion     Completion       `json:"completion_state"`
	Cause          Cause            `json:"termination_cause"`
	DeadlineSource string           `json:"deadline_source,omitempty"`
	CancelSource   string           `json:"cancel_source,omitempty"`
	Detail         string           `json:"detail,omitempty"`
	Timeout        *TimeoutMetadata `json:"timeout,omitempty"`
}

// TimeoutMetadata keeps a timeout visible even after its result was accepted.
type TimeoutMetadata struct {
	Occurred  bool   `json:"occurred"`
	Source    string `json:"source,omitempty"`
	Recovered bool   `json:"recovered,omitempty"`
	Attempts  int    `json:"attempts,omitempty"`
}

// SlotsFromConfig turns the configured metric registry into planned slots. The
// result is the coverage denominator: it is fixed before execution and never
// derived from what a judge returned.
func SlotsFromConfig(metrics []config.MetricConfig) []MetricSlot {
	if len(metrics) == 0 {
		return nil
	}
	out := make([]MetricSlot, 0, len(metrics))
	for _, m := range metrics {
		out = append(out, MetricSlot{
			ID:               m.ID,
			Required:         m.Required,
			MinScore:         m.MinScore,
			EvidenceRequired: m.EvidenceRequired,
		})
	}
	return out
}

// ValidScore reports a finite number in [0,1]. Zero is a valid score; NaN and
// infinities are never clamped into range.
func ValidScore(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0 && *v <= 1
}

// DeriveQuality applies the four threshold rules in order: thresholds plus all
// thresholded values available gives MET/NOT_MET; thresholds plus any thresholded
// value unavailable gives UNKNOWN; no thresholds plus any valid score gives
// NOT_CONFIGURED; no thresholds and no valid scores gives NOT_ASSESSED.
func DeriveQuality(planned []MetricSlot, got []MetricResult) Quality {
	byID := make(map[string]MetricResult, len(got))
	for _, m := range got {
		byID[m.MetricID] = m
	}
	var thresholded []MetricSlot
	var valid int
	for _, s := range planned {
		if s.MinScore != nil {
			thresholded = append(thresholded, s)
		}
		if m, ok := byID[s.ID]; ok && usableScore(m) {
			valid++
		}
	}
	if len(thresholded) > 0 {
		allAvailable, met := true, true
		for _, s := range thresholded {
			m, ok := byID[s.ID]
			if !ok || !usableScore(m) {
				allAvailable = false
				continue
			}
			if *m.Value < *s.MinScore {
				met = false
			}
		}
		if !allAvailable {
			return QualityUnknown
		}
		if met {
			return QualityMet
		}
		return QualityNotMet
	}
	if valid > 0 {
		return QualityNotConfigured
	}
	return QualityNotAssessed
}

func usableScore(m MetricResult) bool {
	return m.Availability == Available && ValidScore(m.Value)
}

// FoldGroup derives one group completion from member completions, keeping the
// most severe member state so truncation never hides an earlier failure:
// FAILED > INCOMPLETE > INTERRUPTED > COMPLETED.
func FoldGroup(members []Completion) (Completion, error) {
	if len(members) == 0 {
		return "", ErrEmptyGroup
	}
	rank := map[Completion]int{
		CompletionFailed: 3, CompletionIncomplete: 2,
		CompletionInterrupted: 1, CompletionFinished: 0,
	}
	best := members[0]
	for _, c := range members[1:] {
		if rank[c] > rank[best] {
			best = c
		}
	}
	return best, nil
}

// EmittedMetric is one producer-supplied metric entry before validation.
type EmittedMetric struct {
	MetricID     string
	Availability string
	Value        *float64
	Reason       string
	Evidence     []string
}

// PayloadProblem is a whole-envelope defect: nothing in the payload can be
// trusted, so every planned slot becomes ERROR.
type PayloadProblem string

const (
	ProblemNone             PayloadProblem = ""
	ProblemUnparseable      PayloadProblem = "unparseable_payload"
	ProblemTopLevelShape    PayloadProblem = "top_level_shape"
	ProblemDuplicateJSONKey PayloadProblem = "duplicate_json_key"
	ProblemUnknownTopField  PayloadProblem = "unknown_top_level_field"
	ProblemSchemaVersion    PayloadProblem = "schema_version"
	ProblemUnboundResult    PayloadProblem = "unbound_or_stale_result"
	ProblemSizeLimit        PayloadProblem = "size_limit"
)

// MaterializeSlots builds exactly one result per planned slot. Unknown ids are
// output diagnostics and never new slots; a duplicated id invalidates its planned
// slot once; the coverage denominator always stays the planned count.
//
// prevented says evaluation never reached a final outcome (gate, cancellation,
// deadline, execution failure), so slots that were never reached become
// NOT_PRODUCED instead of being reported as producer errors. A score that was
// already obtained is never downgraded because evaluation stopped afterwards.
func MaterializeSlots(judgeID string, planned []MetricSlot, emitted []EmittedMetric, problem PayloadProblem, prevented bool) *MetricsEnvelope {
	known := make(map[string]bool, len(planned))
	for _, s := range planned {
		known[s.ID] = true
	}
	grouped := map[string][]EmittedMetric{}
	diagnostics := []string{}
	for _, e := range emitted {
		if !known[e.MetricID] {
			diagnostics = append(diagnostics, "unknown_metric_id: "+e.MetricID)
			continue
		}
		grouped[e.MetricID] = append(grouped[e.MetricID], e)
	}

	out := make([]MetricResult, 0, len(planned))
	for _, s := range planned {
		entries := grouped[s.ID]
		switch {
		case problem != ProblemNone:
			out = append(out, MetricResult{MetricID: s.ID, Availability: Errored, Reason: string(problem)})
		case len(entries) == 0:
			out = append(out, emptySlot(s, prevented))
		case len(entries) > 1:
			diagnostics = append(diagnostics, "duplicate_metric_id: "+s.ID)
			out = append(out, MetricResult{MetricID: s.ID, Availability: Errored, Reason: "duplicate_metric_id"})
		default:
			out = append(out, resolveSlot(s, entries[0], prevented))
		}
	}
	sort.Strings(diagnostics)
	env := &MetricsEnvelope{JudgeID: judgeID, Metrics: out}
	if len(diagnostics) > 0 {
		env.Diagnostics = diagnostics
	}
	env.Quality = DeriveQuality(planned, out)
	env.Coverage = summarizeCoverage(planned, out)
	return env
}

func emptySlot(s MetricSlot, prevented bool) MetricResult {
	if prevented {
		return MetricResult{MetricID: s.ID, Availability: NotProduced, Reason: "evaluation_prevented"}
	}
	if s.Required {
		return MetricResult{MetricID: s.ID, Availability: Missing, Reason: "required_metric_not_emitted"}
	}
	return MetricResult{MetricID: s.ID, Availability: Missing, Reason: "not_emitted"}
}

func resolveSlot(s MetricSlot, e EmittedMetric, prevented bool) MetricResult {
	res := MetricResult{MetricID: s.ID, Evidence: e.Evidence, Reason: e.Reason}
	switch strings.ToUpper(strings.TrimSpace(e.Availability)) {
	case "SKIP", "MISSING":
		// A decline or omission is only meaningful from an evaluation that
		// reached the slot; a stopped evaluation leaves NOT_PRODUCED instead.
		if prevented {
			res.Availability, res.Reason = NotProduced, "evaluation_prevented"
			return res
		}
		if strings.EqualFold(strings.TrimSpace(e.Availability), "SKIP") {
			res.Availability = Declined
			if res.Reason == "" {
				res.Reason = "declined_by_judge"
			}
			return res
		}
		res.Availability = Missing
		if res.Reason == "" {
			res.Reason = "not_emitted"
		}
		return res
	case "ERROR":
		res.Availability = Errored
		if res.Reason == "" {
			res.Reason = "metric_error"
		}
		return res
	case "AVAILABLE", "":
		if !ValidScore(e.Value) {
			res.Availability, res.Reason = Errored, "invalid_value"
			return res
		}
		if s.EvidenceRequired && len(e.Evidence) == 0 {
			res.Availability, res.Reason = Errored, "missing_evidence"
			return res
		}
		res.Availability, res.Value = Available, e.Value
		return res
	default:
		res.Availability, res.Reason = Errored, "unknown_availability_state"
		return res
	}
}

func summarizeCoverage(planned []MetricSlot, got []MetricResult) Coverage {
	c := Coverage{Planned: len(planned)}
	for _, m := range got {
		switch m.Availability {
		case Available:
			c.Available++
		case Missing:
			c.Missing++
		case Errored:
			c.Errored++
		case Declined:
			c.Skipped++
		case NotProduced:
			c.NotProduced++
		}
	}
	if c.Planned > 0 {
		r := float64(c.Available) / float64(c.Planned)
		c.Ratio = &r
	}
	return c
}
