package chatanalyzer

import (
	"context"
	"time"

	"satiksmebot/internal/model"
)

const (
	DryRunStatusWouldApply    = "would_apply"
	DryRunStatusIgnored       = "ignored"
	DryRunStatusUncertain     = "uncertain"
	DryRunStatusModelFailed   = "model_failed"
	DryRunStatusInvalid       = "invalid"
	DryRunStatusLowConfidence = "low_confidence"
)

type DryRunEvaluation struct {
	Status        string        `json:"status"`
	Action        string        `json:"action,omitempty"`
	TargetType    string        `json:"targetType,omitempty"`
	TargetID      string        `json:"targetId,omitempty"`
	Confidence    float64       `json:"confidence,omitempty"`
	Language      string        `json:"language,omitempty"`
	Error         string        `json:"error,omitempty"`
	RawBytes      int           `json:"rawBytes,omitempty"`
	StopCount     int           `json:"stopCandidateCount"`
	VehicleCount  int           `json:"vehicleCandidateCount"`
	IncidentCount int           `json:"incidentCandidateCount"`
	Latency       time.Duration `json:"latency"`
}

func EvaluateDryRun(ctx context.Context, analyzer Analyzer, item model.ChatAnalyzerMessage, candidates CandidateContext, minConfidence float64) DryRunEvaluation {
	started := time.Now()
	out := DryRunEvaluation{
		StopCount:     len(candidates.Stops),
		VehicleCount:  len(candidates.Vehicles),
		IncidentCount: len(candidates.Incidents),
	}
	decision, raw, err := analyzer.Analyze(ctx, item, candidates)
	out.Latency = time.Since(started)
	out.RawBytes = len(raw)
	if err != nil {
		out.Status = DryRunStatusModelFailed
		out.Error = err.Error()
		return out
	}
	out.Action = decision.Action
	out.TargetType = decision.TargetType
	out.TargetID = decision.TargetID
	out.Confidence = decision.Confidence
	out.Language = decision.Language

	normalized, err := normalizeDecision(decision)
	if err != nil {
		out.Status = DryRunStatusInvalid
		out.Error = err.Error()
		return out
	}
	out.Action = normalized.Action
	out.TargetType = normalized.TargetType
	out.TargetID = normalized.TargetID
	out.Confidence = normalized.Confidence
	out.Language = normalized.Language
	if normalized.Action == ActionIgnore {
		out.Status = DryRunStatusIgnored
		return out
	}
	if normalized.Confidence < minConfidence {
		out.Status = DryRunStatusLowConfidence
		out.Error = "low confidence"
		return out
	}
	if _, err := validateTarget(normalized, candidates); err != nil {
		out.Status = DryRunStatusUncertain
		out.Error = err.Error()
		return out
	}
	if _, err := reporterUserID(item); err != nil {
		out.Status = DryRunStatusUncertain
		out.Error = err.Error()
		return out
	}
	out.Status = DryRunStatusWouldApply
	return out
}
