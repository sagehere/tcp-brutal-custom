package main

import (
	"fmt"
	"math"
)

type abComparison struct {
	EpochID                   int64    `json:"epoch_id"`
	Port                      uint16   `json:"port"`
	CanaryPercent             uint8    `json:"canary_percent"`
	DurationSeconds           uint64   `json:"duration_seconds"`
	BaselineConnections       uint64   `json:"baseline_connections"`
	CanaryConnections         uint64   `json:"canary_connections"`
	SelectorFailures          uint64   `json:"selector_failures"`
	ActualCanaryPercent       float64  `json:"actual_canary_percent"`
	AllocationErrorPP         float64  `json:"allocation_error_pp"`
	AllocationZ               float64  `json:"allocation_z"`
	BaselineRetransPercent    float64  `json:"baseline_retrans_percent"`
	CanaryRetransPercent      float64  `json:"canary_retrans_percent"`
	RetransDeltaPP            float64  `json:"retrans_delta_pp"`
	BaselineMeanRTTMS         float64  `json:"baseline_mean_rtt_ms"`
	CanaryMeanRTTMS           float64  `json:"canary_mean_rtt_ms"`
	MeanRTTDeltaPercent       float64  `json:"mean_rtt_delta_percent"`
	BaselineGoodputPerMember  float64  `json:"baseline_goodput_per_member_mbps"`
	CanaryGoodputPerMember    float64  `json:"canary_goodput_per_member_mbps"`
	GoodputPerMemberDeltaPct  float64  `json:"goodput_per_member_delta_percent"`
	BaselineAppRequests       uint64   `json:"baseline_app_requests"`
	CanaryAppRequests         uint64   `json:"canary_app_requests"`
	BaselineAppSuccessPercent float64  `json:"baseline_app_success_percent"`
	CanaryAppSuccessPercent   float64  `json:"canary_app_success_percent"`
	AppSuccessDeltaPP         float64  `json:"app_success_delta_pp"`
	BaselineAppMeanLatencyMS  float64  `json:"baseline_app_mean_latency_ms"`
	CanaryAppMeanLatencyMS    float64  `json:"canary_app_mean_latency_ms"`
	AppMeanLatencyDeltaPct    float64  `json:"app_mean_latency_delta_percent"`
	NetworkReady              bool     `json:"network_ready"`
	ApplicationReady          bool     `json:"application_ready"`
	Reasons                   []string `json:"reasons,omitempty"`
}

type abAnalysisPolicy struct {
	MinDurationSeconds      uint64  `json:"min_duration_seconds"`
	MinConnectionsPerCohort uint64  `json:"min_connections_per_cohort"`
	MinAppRequestsPerCohort uint64  `json:"min_app_requests_per_cohort"`
	MaxSelectorFailurePct   float64 `json:"max_selector_failure_percent"`
	MaxAllocationAbsZ       float64 `json:"max_allocation_abs_z"`
	RequireZeroGapSamples   bool    `json:"require_zero_gap_samples"`
}

func defaultABAnalysisPolicy() abAnalysisPolicy {
	return abAnalysisPolicy{
		MinDurationSeconds:      1800,
		MinConnectionsPerCohort: 200,
		MinAppRequestsPerCohort: 1000,
		MaxSelectorFailurePct:   0.1,
		MaxAllocationAbsZ:       4.0,
		RequireZeroGapSamples:   true,
	}
}

func pctDelta(canary, baseline float64) float64 {
	if baseline == 0 {
		return 0
	}
	return 100 * (canary - baseline) / baseline
}

func buildABComparisons(summaries []abSummary, policy abAnalysisPolicy) []abComparison {
	type pair struct {
		base *abSummary
		can  *abSummary
	}
	byEpoch := map[int64]*pair{}
	order := []int64{}
	for i := range summaries {
		x := &summaries[i]
		p := byEpoch[x.EpochID]
		if p == nil {
			p = &pair{}
			byEpoch[x.EpochID] = p
			order = append(order, x.EpochID)
		}
		if x.Cohort == "baseline" {
			p.base = x
		} else if x.Cohort == "canary" {
			p.can = x
		}
	}
	out := make([]abComparison, 0, len(order))
	for _, id := range order {
		p := byEpoch[id]
		if p.base == nil || p.can == nil {
			continue
		}
		b, c := p.base, p.can
		x := abComparison{
			EpochID: id, Port: b.Port, CanaryPercent: b.CanaryPercent,
			DurationSeconds:     max(b.DurationSeconds, c.DurationSeconds),
			BaselineConnections: b.AssignedConnections, CanaryConnections: c.AssignedConnections,
			SelectorFailures:       max(b.SelectorFailures, c.SelectorFailures),
			BaselineRetransPercent: b.RetransPercent, CanaryRetransPercent: c.RetransPercent,
			RetransDeltaPP:    c.RetransPercent - b.RetransPercent,
			BaselineMeanRTTMS: b.MeanRTTMS, CanaryMeanRTTMS: c.MeanRTTMS,
			MeanRTTDeltaPercent:      pctDelta(c.MeanRTTMS, b.MeanRTTMS),
			BaselineGoodputPerMember: b.GoodputPerMemberMbps, CanaryGoodputPerMember: c.GoodputPerMemberMbps,
			GoodputPerMemberDeltaPct: pctDelta(c.GoodputPerMemberMbps, b.GoodputPerMemberMbps),
			BaselineAppRequests:      b.AppRequests, CanaryAppRequests: c.AppRequests,
			BaselineAppSuccessPercent: b.AppSuccessPercent, CanaryAppSuccessPercent: c.AppSuccessPercent,
			AppSuccessDeltaPP:        c.AppSuccessPercent - b.AppSuccessPercent,
			BaselineAppMeanLatencyMS: b.AppMeanLatencyMS, CanaryAppMeanLatencyMS: c.AppMeanLatencyMS,
			AppMeanLatencyDeltaPct: pctDelta(c.AppMeanLatencyMS, b.AppMeanLatencyMS),
		}
		total := x.BaselineConnections + x.CanaryConnections
		if total > 0 {
			x.ActualCanaryPercent = 100 * float64(x.CanaryConnections) / float64(total)
			x.AllocationErrorPP = x.ActualCanaryPercent - float64(x.CanaryPercent)
			expected := float64(x.CanaryPercent) / 100
			if expected > 0 && expected < 1 {
				se := math.Sqrt(expected * (1 - expected) / float64(total))
				if se > 0 {
					x.AllocationZ = (float64(x.CanaryConnections)/float64(total) - expected) / se
				}
			}
		}

		reasons := []string{}
		if x.CanaryPercent == 0 || x.CanaryPercent == 100 {
			reasons = append(reasons, "epoch is not a two-cohort comparison")
		}
		if x.DurationSeconds < policy.MinDurationSeconds {
			reasons = append(reasons, fmt.Sprintf("duration %ds < %ds", x.DurationSeconds, policy.MinDurationSeconds))
		}
		if x.BaselineConnections < policy.MinConnectionsPerCohort {
			reasons = append(reasons, fmt.Sprintf("baseline connections %d < %d", x.BaselineConnections, policy.MinConnectionsPerCohort))
		}
		if x.CanaryConnections < policy.MinConnectionsPerCohort {
			reasons = append(reasons, fmt.Sprintf("canary connections %d < %d", x.CanaryConnections, policy.MinConnectionsPerCohort))
		}
		if total > 0 && 100*float64(x.SelectorFailures)/float64(total+x.SelectorFailures) > policy.MaxSelectorFailurePct {
			reasons = append(reasons, "selector failure rate exceeds policy")
		}
		if total == 0 {
			reasons = append(reasons, "no assigned connections")
		} else if x.CanaryPercent > 0 && x.CanaryPercent < 100 && math.Abs(x.AllocationZ) > policy.MaxAllocationAbsZ {
			reasons = append(reasons, fmt.Sprintf("allocation |z| %.2f > %.2f", math.Abs(x.AllocationZ), policy.MaxAllocationAbsZ))
		}
		if policy.RequireZeroGapSamples && b.GapSamples+c.GapSamples > 0 {
			reasons = append(reasons, "gap samples present")
		}
		x.NetworkReady = len(reasons) == 0
		x.Reasons = reasons

		appReasons := append([]string(nil), reasons...)
		if b.AppRequests < policy.MinAppRequestsPerCohort {
			appReasons = append(appReasons, fmt.Sprintf("baseline app requests %d < %d", b.AppRequests, policy.MinAppRequestsPerCohort))
		}
		if c.AppRequests < policy.MinAppRequestsPerCohort {
			appReasons = append(appReasons, fmt.Sprintf("canary app requests %d < %d", c.AppRequests, policy.MinAppRequestsPerCohort))
		}
		x.ApplicationReady = len(appReasons) == 0
		if !x.ApplicationReady && x.NetworkReady {
			x.Reasons = appReasons
		}
		out = append(out, x)
	}
	return out
}
