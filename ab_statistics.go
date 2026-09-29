package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
)

const abBootstrapReplicates = 2000

type abExperimentPlan struct {
	Alpha                     float64 `json:"alpha"`
	Power                     float64 `json:"power"`
	ExpectedAppSuccessPercent float64 `json:"expected_app_success_percent"`
	AppSuccessNIMarginPP      float64 `json:"app_success_ni_margin_pp"`
	MaxRetransDeltaPP         float64 `json:"max_retrans_delta_pp"`
	MaxMeanRTTDeltaPercent    float64 `json:"max_mean_rtt_delta_percent"`
	MinGoodputDeltaPercent    float64 `json:"min_goodput_delta_percent"`
	BootstrapBlockMinutes     int     `json:"bootstrap_block_minutes"`
}

type abStoredPlan struct {
	Plan        abExperimentPlan
	Predeclared bool
}

func defaultABExperimentPlan() abExperimentPlan {
	return abExperimentPlan{
		Alpha:                     0.05,
		Power:                     0.80,
		ExpectedAppSuccessPercent: 99.0,
		AppSuccessNIMarginPP:      0.5,
		MaxRetransDeltaPP:         0.5,
		MaxMeanRTTDeltaPercent:    10.0,
		MinGoodputDeltaPercent:    -10.0,
		BootstrapBlockMinutes:     5,
	}
}

func effectiveABExperimentPlan(in *abExperimentPlan) (abExperimentPlan, error) {
	if in == nil {
		return defaultABExperimentPlan(), nil
	}
	p := *in
	switch {
	case p.Alpha <= 0 || p.Alpha > 0.2:
		return p, fmt.Errorf("analysis alpha must be in (0,0.2]")
	case p.Power < 0.5 || p.Power >= 1:
		return p, fmt.Errorf("analysis power must be in [0.5,1)")
	case p.ExpectedAppSuccessPercent <= 0 || p.ExpectedAppSuccessPercent >= 100:
		return p, fmt.Errorf("expected app success percent must be in (0,100)")
	case p.AppSuccessNIMarginPP <= 0 || p.AppSuccessNIMarginPP > 20:
		return p, fmt.Errorf("application non-inferiority margin must be in (0,20] percentage points")
	case p.MaxRetransDeltaPP < 0 || p.MaxRetransDeltaPP > 100:
		return p, fmt.Errorf("max retransmission delta must be in [0,100] percentage points")
	case p.MaxMeanRTTDeltaPercent < 0 || p.MaxMeanRTTDeltaPercent > 500:
		return p, fmt.Errorf("max RTT delta must be in [0,500] percent")
	case p.MinGoodputDeltaPercent < -100 || p.MinGoodputDeltaPercent > 100:
		return p, fmt.Errorf("min goodput delta must be in [-100,100] percent")
	case p.BootstrapBlockMinutes < 1 || p.BootstrapBlockMinutes > 60:
		return p, fmt.Errorf("bootstrap block minutes must be in [1,60]")
	}
	return p, nil
}

type abCI struct {
	Available bool    `json:"available"`
	Estimate  float64 `json:"estimate"`
	Lower     float64 `json:"lower"`
	Upper     float64 `json:"upper"`
}

type abSampleTarget struct {
	Available bool   `json:"available"`
	Baseline  uint64 `json:"baseline"`
	Canary    uint64 `json:"canary"`
	Total     uint64 `json:"total"`
}

type abAppInference struct {
	Available            bool           `json:"available"`
	BaselineSuccess      abCI           `json:"baseline_success_percent"`
	CanarySuccess        abCI           `json:"canary_success_percent"`
	DifferencePP         abCI           `json:"difference_pp"`
	NonInferiorityMargin float64        `json:"non_inferiority_margin_pp"`
	NonInferiorityPass   bool           `json:"non_inferiority_pass"`
	SampleTarget         abSampleTarget `json:"sample_target"`
	SampleTargetMet      bool           `json:"sample_target_met"`
}

type abNetworkInference struct {
	Available       bool `json:"available"`
	PairedMinutes   int  `json:"paired_minutes"`
	BlockMinutes    int  `json:"block_minutes"`
	Replicates      int  `json:"replicates"`
	RetransDeltaPP  abCI `json:"retrans_delta_pp"`
	RTTDeltaPercent abCI `json:"mean_rtt_delta_percent"`
	GoodputDeltaPct abCI `json:"goodput_per_member_delta_percent"`
}

type abEpochAnalysis struct {
	EpochID          int64              `json:"epoch_id"`
	Port             uint16             `json:"port"`
	CanaryPercent    uint8              `json:"canary_percent"`
	Plan             abExperimentPlan   `json:"plan"`
	PlanPredeclared  bool               `json:"plan_predeclared"`
	Application      abAppInference     `json:"application"`
	Network          abNetworkInference `json:"network"`
	State            string             `json:"state"`
	NextStagePercent uint8              `json:"next_stage_percent,omitempty"`
	Reasons          []string           `json:"reasons,omitempty"`
}

func normalQuantile(p float64) float64 {
	return math.Sqrt2 * math.Erfinv(2*p-1)
}

func wilsonPercent(success, total uint64, alpha float64) abCI {
	if total == 0 {
		return abCI{}
	}
	z := normalQuantile(1 - alpha/2)
	n := float64(total)
	phat := float64(success) / n
	den := 1 + z*z/n
	center := (phat + z*z/(2*n)) / den
	half := z * math.Sqrt(phat*(1-phat)/n+z*z/(4*n*n)) / den
	return abCI{Available: true, Estimate: 100 * phat, Lower: 100 * math.Max(0, center-half), Upper: 100 * math.Min(1, center+half)}
}

func newcombeDifferencePP(canSuccess, canTotal, baseSuccess, baseTotal uint64, alpha float64) abCI {
	if canTotal == 0 || baseTotal == 0 {
		return abCI{}
	}
	can := wilsonPercent(canSuccess, canTotal, alpha)
	base := wilsonPercent(baseSuccess, baseTotal, alpha)
	diff := can.Estimate - base.Estimate
	lower := diff - math.Sqrt(math.Pow(can.Estimate-can.Lower, 2)+math.Pow(base.Upper-base.Estimate, 2))
	upper := diff + math.Sqrt(math.Pow(can.Upper-can.Estimate, 2)+math.Pow(base.Estimate-base.Lower, 2))
	return abCI{Available: true, Estimate: diff, Lower: lower, Upper: upper}
}

func appSampleTarget(plan abExperimentPlan, canaryPercent uint8) abSampleTarget {
	if canaryPercent == 0 || canaryPercent == 100 {
		return abSampleTarget{}
	}
	wc := float64(canaryPercent) / 100
	wb := 1 - wc
	p := plan.ExpectedAppSuccessPercent / 100
	d := plan.AppSuccessNIMarginPP / 100
	zAlpha := normalQuantile(1 - plan.Alpha/2)
	zPower := normalQuantile(plan.Power)
	total := math.Ceil(math.Pow(zAlpha+zPower, 2) * p * (1 - p) * (1/wb + 1/wc) / (d * d))
	if total < 2 {
		total = 2
	}
	base := uint64(math.Ceil(total * wb))
	canary := uint64(math.Ceil(total * wc))
	return abSampleTarget{Available: true, Baseline: base, Canary: canary, Total: base + canary}
}

func appInference(c abComparison, base, canary abSummary, plan abExperimentPlan) abAppInference {
	target := appSampleTarget(plan, c.CanaryPercent)
	out := abAppInference{
		BaselineSuccess:      wilsonPercent(base.AppSuccess, base.AppRequests, plan.Alpha),
		CanarySuccess:        wilsonPercent(canary.AppSuccess, canary.AppRequests, plan.Alpha),
		DifferencePP:         newcombeDifferencePP(canary.AppSuccess, canary.AppRequests, base.AppSuccess, base.AppRequests, plan.Alpha),
		NonInferiorityMargin: plan.AppSuccessNIMarginPP,
		SampleTarget:         target,
	}
	out.Available = out.DifferencePP.Available
	out.SampleTargetMet = target.Available && base.AppRequests >= target.Baseline && canary.AppRequests >= target.Canary
	out.NonInferiorityPass = out.Available && out.DifferencePP.Lower >= -plan.AppSuccessNIMarginPP
	return out
}

type abMinutePair struct {
	Time       int64
	RetransPP  float64
	RTTDelta   float64
	GoodputPct float64
	RetransOK  bool
	RTTOK      bool
	GoodputOK  bool
}

type abMinuteAgg struct {
	Sent          uint64
	Acked         uint64
	Retrans       uint64
	RTTSum        uint64
	RTTSamples    uint64
	MemberSeconds uint64
	Gap           bool
}

func minutePairs(samples []abCohortSample, epochID int64) []abMinutePair {
	type cohorts struct {
		base abMinuteAgg
		can  abMinuteAgg
	}
	bins := map[int64]*cohorts{}
	for _, x := range samples {
		if x.EpochID != epochID {
			continue
		}
		b := bins[x.Time]
		if b == nil {
			b = &cohorts{}
			bins[x.Time] = b
		}
		var a *abMinuteAgg
		if x.Cohort == "baseline" {
			a = &b.base
		} else if x.Cohort == "canary" {
			a = &b.can
		} else {
			continue
		}
		a.Sent += x.Sent
		a.Acked += x.Acked
		a.Retrans += x.Retrans
		a.RTTSum += x.RTTSum
		a.RTTSamples += x.RTTSamples
		a.MemberSeconds += x.MemberSeconds
		a.Gap = a.Gap || x.Gap
	}
	times := make([]int64, 0, len(bins))
	for ts := range bins {
		times = append(times, ts)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	out := make([]abMinutePair, 0, len(times))
	for _, ts := range times {
		x := bins[ts]
		if x.base.Gap || x.can.Gap {
			continue
		}
		p := abMinutePair{Time: ts}
		if x.base.Sent > 0 && x.can.Sent > 0 {
			br := 100 * float64(x.base.Retrans) / float64(x.base.Sent)
			cr := 100 * float64(x.can.Retrans) / float64(x.can.Sent)
			p.RetransPP, p.RetransOK = cr-br, true
		}
		if x.base.RTTSamples > 0 && x.can.RTTSamples > 0 {
			br := float64(x.base.RTTSum) / float64(x.base.RTTSamples)
			cr := float64(x.can.RTTSum) / float64(x.can.RTTSamples)
			if br > 0 {
				p.RTTDelta, p.RTTOK = 100*(cr-br)/br, true
			}
		}
		if x.base.MemberSeconds > 0 && x.can.MemberSeconds > 0 {
			bg := float64(x.base.Acked) * 8 / float64(x.base.MemberSeconds) / 1e6
			cg := float64(x.can.Acked) * 8 / float64(x.can.MemberSeconds) / 1e6
			if bg > 0 {
				p.GoodputPct, p.GoodputOK = 100*(cg-bg)/bg, true
			}
		}
		out = append(out, p)
	}
	return out
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	pos := q * float64(len(sorted)-1)
	i := int(math.Floor(pos))
	f := pos - float64(i)
	if i+1 >= len(sorted) {
		return sorted[i]
	}
	return sorted[i]*(1-f) + sorted[i+1]*f
}

func blockBootstrapCI(values []float64, block, replicates int, alpha float64, seed int64) abCI {
	if len(values) < max(2*block, 10) || block < 1 || replicates < 100 {
		return abCI{}
	}
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	rng := rand.New(rand.NewSource(seed))
	boots := make([]float64, replicates)
	n := len(values)
	for r := 0; r < replicates; r++ {
		sum, count := 0.0, 0
		for count < n {
			start := rng.Intn(n)
			for j := 0; j < block && count < n; j++ {
				sum += values[(start+j)%n]
				count++
			}
		}
		boots[r] = sum / float64(n)
	}
	sort.Float64s(boots)
	return abCI{Available: true, Estimate: mean, Lower: percentile(boots, alpha/2), Upper: percentile(boots, 1-alpha/2)}
}

func networkInference(samples []abCohortSample, epochID int64, plan abExperimentPlan) abNetworkInference {
	pairs := minutePairs(samples, epochID)
	retrans := make([]float64, 0, len(pairs))
	rtt := make([]float64, 0, len(pairs))
	goodput := make([]float64, 0, len(pairs))
	for _, p := range pairs {
		if p.RetransOK {
			retrans = append(retrans, p.RetransPP)
		}
		if p.RTTOK {
			rtt = append(rtt, p.RTTDelta)
		}
		if p.GoodputOK {
			goodput = append(goodput, p.GoodputPct)
		}
	}
	block := plan.BootstrapBlockMinutes
	out := abNetworkInference{
		PairedMinutes:   len(pairs),
		BlockMinutes:    block,
		Replicates:      abBootstrapReplicates,
		RetransDeltaPP:  blockBootstrapCI(retrans, block, abBootstrapReplicates, plan.Alpha, epochID*31+1),
		RTTDeltaPercent: blockBootstrapCI(rtt, block, abBootstrapReplicates, plan.Alpha, epochID*31+2),
		GoodputDeltaPct: blockBootstrapCI(goodput, block, abBootstrapReplicates, plan.Alpha, epochID*31+3),
	}
	out.Available = out.RetransDeltaPP.Available && out.RTTDeltaPercent.Available && out.GoodputDeltaPct.Available
	return out
}

func nextABStage(current uint8) uint8 {
	for _, p := range []uint8{5, 10, 25, 50, 100} {
		if p > current {
			return p
		}
	}
	return 0
}

func buildABEpochAnalyses(epochs []abEpoch, summaries []abSummary, comparisons []abComparison, minuteSamples []abCohortSample, plans map[int64]abStoredPlan) []abEpochAnalysis {
	bySummary := map[string]abSummary{}
	for _, s := range summaries {
		bySummary[fmt.Sprintf("%d/%s", s.EpochID, s.Cohort)] = s
	}
	byComp := map[int64]abComparison{}
	for _, c := range comparisons {
		byComp[c.EpochID] = c
	}
	out := make([]abEpochAnalysis, 0, len(epochs))
	for _, e := range epochs {
		stored, ok := plans[e.ID]
		if !ok {
			stored = abStoredPlan{Plan: defaultABExperimentPlan(), Predeclared: false}
		}
		c, haveComp := byComp[e.ID]
		base := bySummary[fmt.Sprintf("%d/baseline", e.ID)]
		canary := bySummary[fmt.Sprintf("%d/canary", e.ID)]
		x := abEpochAnalysis{
			EpochID: e.ID, Port: e.Port, CanaryPercent: e.CanaryPercent,
			Plan: stored.Plan, PlanPredeclared: stored.Predeclared,
		}
		if haveComp {
			x.Application = appInference(c, base, canary, stored.Plan)
			x.Network = networkInference(minuteSamples, e.ID, stored.Plan)
		}
		switch {
		case e.CanaryPercent == 0 || e.CanaryPercent == 100:
			x.State = "not_comparable"
			x.Reasons = append(x.Reasons, "epoch is not a two-cohort comparison")
		case !stored.Predeclared:
			x.State = "collecting"
			x.Reasons = append(x.Reasons, "analysis plan was not predeclared for this epoch")
		case !haveComp || !c.NetworkReady:
			x.State = "collecting"
			x.Reasons = append(x.Reasons, "network readiness requirements are not yet met")
		case !x.Network.Available:
			x.State = "collecting"
			x.Reasons = append(x.Reasons, fmt.Sprintf("paired minute samples are insufficient for %d-minute block bootstrap", stored.Plan.BootstrapBlockMinutes))
		default:
			networkPass := x.Network.RetransDeltaPP.Upper <= stored.Plan.MaxRetransDeltaPP &&
				x.Network.RTTDeltaPercent.Upper <= stored.Plan.MaxMeanRTTDeltaPercent &&
				x.Network.GoodputDeltaPct.Lower >= stored.Plan.MinGoodputDeltaPercent
			if !networkPass {
				x.State = "guardrail_review"
				x.Reasons = append(x.Reasons, "one or more network confidence intervals cross a predeclared guardrail")
				break
			}
			appPresent := base.AppRequests > 0 || canary.AppRequests > 0
			if !appPresent {
				x.State = "network_review_only"
				x.Reasons = append(x.Reasons, "application metrics are not available")
				break
			}
			if !c.ApplicationReady || !x.Application.SampleTargetMet {
				x.State = "collecting"
				x.Reasons = append(x.Reasons, "application readiness or precomputed sample target is not yet met")
				break
			}
			if !x.Application.NonInferiorityPass {
				x.State = "guardrail_review"
				x.Reasons = append(x.Reasons, "application success non-inferiority confidence interval crosses the predeclared margin")
				break
			}
			x.State = "eligible_review"
			x.NextStagePercent = nextABStage(e.CanaryPercent)
			x.Reasons = append(x.Reasons, "predeclared data-quality, sample-size, and guardrail checks are satisfied")
		}
		out = append(out, x)
	}
	return out
}