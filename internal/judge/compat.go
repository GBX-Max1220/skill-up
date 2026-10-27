package judge

// Definition compatibility for named metrics (#317). Mandatory equality covers
// the scoring definition itself; provider, model and revision are provenance and
// produce an advisory only, because different identifiers can name the same
// underlying model and identical aliases can drift.
//
// The decision is pure logic so comparisons can consume it in a later bounded PR
// without re-deriving the rules.

// ScoringDefinition is the comparison-relevant identity of one metric series.
type ScoringDefinition struct {
	JudgeID  string
	MetricID string
	// RubricHash covers exact rubric text, anchors and version.
	RubricHash string
	// ScaleHash covers the declared scale, direction and range.
	ScaleHash string
	// Provenance only: never part of mandatory equality.
	Provider string
	Model    string
	// ModelRevision is recorded only when a provider explicitly supplied a
	// concrete version. An empty value means unknown and is never inferred
	// from an alias.
	ModelRevision string
}

// Compatibility is the outcome of comparing two definitions.
type Compatibility struct {
	Comparable bool
	// Reasons blocks pairing: mismatched scoring definition or scale.
	Reasons []string
	// Advisories inform, never block.
	Advisories []string
}

// CompareScoringDefinitions reports whether two metric series may be paired.
func CompareScoringDefinitions(a, b ScoringDefinition) Compatibility {
	out := Compatibility{Comparable: true}
	if a.JudgeID != b.JudgeID {
		out.Reasons = append(out.Reasons, "judge_id_mismatch")
	}
	if a.MetricID != b.MetricID {
		out.Reasons = append(out.Reasons, "metric_id_mismatch")
	}
	if a.RubricHash != b.RubricHash {
		out.Reasons = append(out.Reasons, "rubric_mismatch")
	}
	if a.ScaleHash != b.ScaleHash {
		out.Reasons = append(out.Reasons, "scoring_scale_mismatch")
	}
	if len(out.Reasons) > 0 {
		out.Comparable = false
	}
	if a.Provider != b.Provider {
		out.Advisories = append(out.Advisories, "provider_differs")
	}
	if a.Model != b.Model {
		out.Advisories = append(out.Advisories, "model_alias_differs")
	}
	if a.ModelRevision == "" || b.ModelRevision == "" {
		out.Advisories = append(out.Advisories, "model_revision_unknown")
	} else if a.ModelRevision != b.ModelRevision {
		out.Advisories = append(out.Advisories, "model_revision_differs")
	}
	return out
}
