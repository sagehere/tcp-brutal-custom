package main

import (
	"math"
	"testing"
)

func TestWilsonAndNewcombeIntervals(t *testing.T) {
	w := wilsonPercent(990, 1000, 0.05)
	if !w.Available || math.Abs(w.Estimate-99) > 1e-9 {
		t.Fatalf("Wilson interval=%+v", w)
	}
	if !(w.Lower < w.Estimate && w.Upper > w.Estimate) {
		t.Fatalf("Wilson bounds do not contain estimate: %+v", w)
	}
	d := newcombeDifferencePP(995, 1000, 990, 1000, 0.05)
	if !d.Available || math.Abs(d.Estimate-0.5) > 1e-9 {
		t.Fatalf("Newcombe difference=%+v", d)
	}
	if !(d.Lower < d.Estimate && d.Upper > d.Estimate) {
		t.Fatalf("Newcombe bounds do not contain estimate: %+v", d)
	}
}

func TestAppSampleTargetRespectsAllocation(t *testing.T) {
	p := defaultABExperimentPlan()
	equal := appSampleTarget(p, 50)
	smallCanary := appSampleTarget(p, 5)
	if !equal.Available || equal.Baseline == 0 || equal.Canary == 0 {
		t.Fatalf("equal allocation target=%+v", equal)
	}
	if !smallCanary.Available || smallCanary.Total <= equal.Total || smallCanary.Baseline <= equal.Baseline {
		t.Fatalf("5%% allocation target=%+v equal=%+v", smallCanary, equal)
	}
	if smallCanary.Baseline <= 10*smallCanary.Canary {
		t.Fatalf("5%% target does not reflect the configured allocation: %+v", smallCanary)
	}
}

func constantMinuteSamples(epoch int64, retransCanary uint64) []abCohortSample {
	out := make([]abCohortSample, 0, 80)
	for i := 0; i < 40; i++ {
		ts := int64(1_700_000_000 + i*60)
		out = append(out,
			abCohortSample{
				Time: ts, EpochID: epoch, Port: 443, Cohort: "baseline", Group: 1,
				Duration: 60, MemberSeconds: 60, Sent: 1_000_000, Acked: 900_000,
				Retrans: 10_000, RTTSum: 5_000_000, RTTSamples: 100,
			},
			abCohortSample{
				Time: ts, EpochID: epoch, Port: 443, Cohort: "canary", Group: 2,
				Duration: 60, MemberSeconds: 60, Sent: 1_000_000, Acked: 945_000,
				Retrans: retransCanary, RTTSum: 4_800_000, RTTSamples: 100,
			},
		)
	}
	return out
}

func TestBlockBootstrapConstantSeries(t *testing.T) {
	values := make([]float64, 40)
	for i := range values {
		values[i] = -0.5
	}
	got := blockBootstrapCI(values, 5, 500, 0.05, 42)
	if !got.Available || got.Estimate != -0.5 || got.Lower != -0.5 || got.Upper != -0.5 {
		t.Fatalf("constant bootstrap=%+v", got)
	}
	if again := blockBootstrapCI(values, 5, 500, 0.05, 42); again != got {
		t.Fatalf("bootstrap is not deterministic: %+v vs %+v", got, again)
	}
}

func healthySummaries(epoch int64) []abSummary {
	return []abSummary{
		{
			EpochID: epoch, Port: 443, Cohort: "baseline", CanaryPercent: 50,
			DurationSeconds: 3600, AssignedConnections: 1000, AppRequests: 10000,
			AppSuccess: 9900, AppErrors: 100, GapSamples: 0,
		},
		{
			EpochID: epoch, Port: 443, Cohort: "canary", CanaryPercent: 50,
			DurationSeconds: 3600, AssignedConnections: 1000, AppRequests: 10000,
			AppSuccess: 9910, AppErrors: 90, GapSamples: 0,
		},
	}
}

func TestEpochAnalysisEligibleForManualReview(t *testing.T) {
	const epoch = int64(77)
	summaries := healthySummaries(epoch)
	comparisons := buildABComparisons(summaries, defaultABAnalysisPolicy())
	plan := defaultABExperimentPlan()
	got := buildABEpochAnalyses(
		[]abEpoch{{ID: epoch, Port: 443, CanaryPercent: 50}},
		summaries,
		comparisons,
		constantMinuteSamples(epoch, 5_000),
		map[int64]abStoredPlan{epoch: {Plan: plan, Predeclared: true}},
	)
	if len(got) != 1 {
		t.Fatalf("analyses=%+v", got)
	}
	x := got[0]
	if x.State != "eligible_review" || x.NextStagePercent != 100 {
		t.Fatalf("eligible analysis=%+v", x)
	}
	if !x.Network.Available || !x.Application.Available || !x.Application.SampleTargetMet || !x.Application.NonInferiorityPass {
		t.Fatalf("inference not ready: %+v", x)
	}
	if x.Network.RetransDeltaPP.Upper > plan.MaxRetransDeltaPP {
		t.Fatalf("unexpected retrans upper bound: %+v", x.Network.RetransDeltaPP)
	}
}

func TestEpochAnalysisFlagsNetworkGuardrail(t *testing.T) {
	const epoch = int64(78)
	summaries := healthySummaries(epoch)
	comparisons := buildABComparisons(summaries, defaultABAnalysisPolicy())
	plan := defaultABExperimentPlan()
	got := buildABEpochAnalyses(
		[]abEpoch{{ID: epoch, Port: 443, CanaryPercent: 50}},
		summaries,
		comparisons,
		constantMinuteSamples(epoch, 20_000),
		map[int64]abStoredPlan{epoch: {Plan: plan, Predeclared: true}},
	)
	if len(got) != 1 || got[0].State != "guardrail_review" {
		t.Fatalf("guardrail analysis=%+v", got)
	}
	if got[0].Network.RetransDeltaPP.Lower <= plan.MaxRetransDeltaPP {
		t.Fatalf("test did not create a clear retrans regression: %+v", got[0].Network.RetransDeltaPP)
	}
}

func TestEpochAnalysisRequiresPredeclaredPlan(t *testing.T) {
	const epoch = int64(79)
	summaries := healthySummaries(epoch)
	comparisons := buildABComparisons(summaries, defaultABAnalysisPolicy())
	got := buildABEpochAnalyses(
		[]abEpoch{{ID: epoch, Port: 443, CanaryPercent: 50}},
		summaries,
		comparisons,
		constantMinuteSamples(epoch, 5_000),
		nil,
	)
	if len(got) != 1 || got[0].State != "collecting" || got[0].PlanPredeclared {
		t.Fatalf("legacy epoch analysis=%+v", got)
	}
}