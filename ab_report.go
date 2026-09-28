package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func buildABReport(h *history, port uint16, from, to int64, tier string) ([]byte, error) {
	if port == 0 {
		return nil, fmt.Errorf("port required")
	}
	if from <= 0 || to <= from || to-from > 366*86400 {
		return nil, fmt.Errorf("invalid time range")
	}
	if tier == "" {
		tier = "minute"
	}
	epochs, err := h.abEpochs(port, from, to)
	if err != nil {
		return nil, err
	}
	samples, err := h.abSamples(tier, port, from, to)
	if err != nil {
		return nil, err
	}
	selectors, err := h.abSelectorSamples(tier, port, from, to)
	if err != nil {
		return nil, err
	}
	summaries, err := h.abSummaries(port, from, to)
	if err != nil {
		return nil, err
	}
	policy := defaultABAnalysisPolicy()
	comparisons := buildABComparisons(summaries, policy)
	appSamples, err := h.abAppSamples(tier, port, from, to)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var checksums []string
	writeFile := func(name string, data []byte) error {
		sum := sha256.Sum256(data)
		checksums = append(checksums, fmt.Sprintf("%x  %s", sum, name))
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	if err := writeFile("manifest.json", marshalABManifest(port, from, to, tier, epochs, summaries)); err != nil {
		zw.Close()
		return nil, err
	}

	csvBytes := func(header []string, rows [][]string) ([]byte, error) {
		var b bytes.Buffer
		w := csv.NewWriter(&b)
		if err := w.Write(header); err != nil {
			return nil, err
		}
		for _, row := range rows {
			if err := w.Write(row); err != nil {
				return nil, err
			}
		}
		w.Flush()
		return b.Bytes(), w.Error()
	}

	var epochRows [][]string
	for _, e := range epochs {
		epochRows = append(epochRows, []string{
			strconv.FormatInt(e.ID, 10), strconv.Itoa(int(e.Port)),
			strconv.FormatInt(e.Started, 10), strconv.FormatInt(e.Ended, 10),
			strconv.Itoa(int(e.CanaryPercent)), strconv.FormatFloat(e.RateMbps, 'f', 3, 64),
			strconv.Itoa(int(e.Gain)), e.CodeVersion, e.Reason,
		})
	}
	b, err := csvBytes([]string{"epoch_id", "port", "started_unix", "ended_unix", "canary_percent", "rate_mbps", "gain", "code_version", "reason"}, epochRows)
	if err != nil {
		zw.Close()
		return nil, err
	}
	if err = writeFile("epochs.csv", b); err != nil {
		zw.Close()
		return nil, err
	}

	var sampleRows [][]string
	for _, x := range samples {
		sampleRows = append(sampleRows, []string{
			strconv.FormatInt(x.Time, 10), strconv.FormatInt(x.EpochID, 10), strconv.Itoa(int(x.Port)), x.Cohort,
			strconv.FormatUint(x.Group, 10), strconv.FormatUint(x.Duration, 10), strconv.FormatUint(x.MemberSeconds, 10),
			strconv.FormatUint(x.Sent, 10), strconv.FormatUint(x.Acked, 10), strconv.FormatUint(x.Retrans, 10),
			strconv.Itoa(int(x.Members)), strconv.FormatUint(x.RTTSum, 10), strconv.FormatUint(x.RTTSamples, 10),
			strconv.Itoa(int(x.RTTMax)), strconv.FormatBool(x.Gap),
		})
	}
	b, err = csvBytes([]string{"time_unix", "epoch_id", "port", "cohort", "group_id", "duration_seconds", "member_seconds", "sent_bytes", "acked_bytes", "retrans_bytes", "members", "rtt_sum_us", "rtt_samples", "rtt_max_us", "gap"}, sampleRows)
	if err != nil {
		zw.Close()
		return nil, err
	}
	if err = writeFile("cohort_samples.csv", b); err != nil {
		zw.Close()
		return nil, err
	}

	var selectorRows [][]string
	for _, x := range selectors {
		selectorRows = append(selectorRows, []string{
			strconv.FormatInt(x.Time, 10), strconv.FormatInt(x.EpochID, 10), strconv.Itoa(int(x.Port)),
			strconv.FormatUint(x.Baseline, 10), strconv.FormatUint(x.Canary, 10), strconv.FormatUint(x.Failure, 10), strconv.FormatBool(x.Gap),
		})
	}
	b, err = csvBytes([]string{"time_unix", "epoch_id", "port", "baseline_connections", "canary_connections", "selector_failures", "gap"}, selectorRows)
	if err != nil {
		zw.Close()
		return nil, err
	}
	if err = writeFile("selector_samples.csv", b); err != nil {
		zw.Close()
		return nil, err
	}

	var appRows [][]string
	for _, x := range appSamples {
		appRows = append(appRows, []string{
			strconv.FormatInt(x.Time, 10), strconv.FormatInt(x.EpochID, 10), strconv.Itoa(int(x.Port)), x.Cohort, x.Source,
			strconv.FormatUint(x.Requests, 10), strconv.FormatUint(x.Success, 10), strconv.FormatUint(x.Errors, 10),
			strconv.FormatUint(x.LatencySumUS, 10), strconv.FormatUint(x.LatencySamples, 10), strconv.FormatUint(x.LatencyMaxUS, 10),
		})
	}
	b, err = csvBytes([]string{"time_unix", "epoch_id", "port", "cohort", "source", "requests", "success", "errors", "latency_sum_us", "latency_samples", "latency_max_us"}, appRows)
	if err != nil {
		zw.Close()
		return nil, err
	}
	if err = writeFile("app_samples.csv", b); err != nil {
		zw.Close()
		return nil, err
	}

	var summaryRows [][]string
	for _, x := range summaries {
		summaryRows = append(summaryRows, []string{
			strconv.FormatInt(x.EpochID, 10), strconv.Itoa(int(x.Port)), x.Cohort, strconv.Itoa(int(x.CanaryPercent)),
			strconv.FormatUint(x.DurationSeconds, 10), strconv.FormatUint(x.MemberSeconds, 10),
			strconv.FormatUint(x.SentBytes, 10), strconv.FormatUint(x.AckedBytes, 10), strconv.FormatUint(x.RetransBytes, 10),
			strconv.FormatUint(x.AssignedConnections, 10), strconv.FormatUint(x.SelectorFailures, 10), strconv.FormatUint(x.RTTSamples, 10),
			strconv.FormatFloat(x.MeanRTTMS, 'f', 4, 64), strconv.FormatFloat(x.MaxRTTMS, 'f', 4, 64),
			strconv.FormatFloat(x.RetransPercent, 'f', 6, 64), strconv.FormatFloat(x.GoodputMbps, 'f', 6, 64),
			strconv.FormatFloat(x.GoodputPerMemberMbps, 'f', 6, 64), strconv.FormatUint(x.AppRequests, 10),
			strconv.FormatUint(x.AppSuccess, 10), strconv.FormatUint(x.AppErrors, 10),
			strconv.FormatFloat(x.AppSuccessPercent, 'f', 6, 64), strconv.FormatFloat(x.AppMeanLatencyMS, 'f', 4, 64),
			strconv.FormatFloat(x.AppMaxLatencyMS, 'f', 4, 64), strconv.FormatUint(x.GapSamples, 10),
		})
	}
	b, err = csvBytes([]string{"epoch_id", "port", "cohort", "canary_percent", "duration_seconds", "member_seconds", "sent_bytes", "acked_bytes", "retrans_bytes", "assigned_connections", "selector_failures", "rtt_samples", "mean_rtt_ms", "max_rtt_ms", "retrans_percent", "goodput_mbps", "goodput_per_member_mbps", "app_requests", "app_success", "app_errors", "app_success_percent", "app_mean_latency_ms", "app_max_latency_ms", "gap_samples"}, summaryRows)
	if err != nil {
		zw.Close()
		return nil, err
	}
	if err = writeFile("summary.csv", b); err != nil {
		zw.Close()
		return nil, err
	}

	var comparisonRows [][]string
	for _, x := range comparisons {
		comparisonRows = append(comparisonRows, []string{
			strconv.FormatInt(x.EpochID, 10), strconv.Itoa(int(x.Port)), strconv.Itoa(int(x.CanaryPercent)),
			strconv.FormatUint(x.DurationSeconds, 10), strconv.FormatUint(x.BaselineConnections, 10), strconv.FormatUint(x.CanaryConnections, 10),
			strconv.FormatUint(x.SelectorFailures, 10), strconv.FormatFloat(x.ActualCanaryPercent, 'f', 4, 64),
			strconv.FormatFloat(x.AllocationErrorPP, 'f', 4, 64), strconv.FormatFloat(x.AllocationZ, 'f', 4, 64),
			strconv.FormatFloat(x.BaselineRetransPercent, 'f', 6, 64), strconv.FormatFloat(x.CanaryRetransPercent, 'f', 6, 64),
			strconv.FormatFloat(x.RetransDeltaPP, 'f', 6, 64), strconv.FormatFloat(x.BaselineMeanRTTMS, 'f', 4, 64),
			strconv.FormatFloat(x.CanaryMeanRTTMS, 'f', 4, 64), strconv.FormatFloat(x.MeanRTTDeltaPercent, 'f', 4, 64),
			strconv.FormatFloat(x.BaselineGoodputPerMember, 'f', 6, 64), strconv.FormatFloat(x.CanaryGoodputPerMember, 'f', 6, 64),
			strconv.FormatFloat(x.GoodputPerMemberDeltaPct, 'f', 4, 64), strconv.FormatUint(x.BaselineAppRequests, 10),
			strconv.FormatUint(x.CanaryAppRequests, 10), strconv.FormatFloat(x.BaselineAppSuccessPercent, 'f', 6, 64),
			strconv.FormatFloat(x.CanaryAppSuccessPercent, 'f', 6, 64), strconv.FormatFloat(x.AppSuccessDeltaPP, 'f', 6, 64),
			strconv.FormatFloat(x.BaselineAppMeanLatencyMS, 'f', 4, 64), strconv.FormatFloat(x.CanaryAppMeanLatencyMS, 'f', 4, 64),
			strconv.FormatFloat(x.AppMeanLatencyDeltaPct, 'f', 4, 64), strconv.FormatBool(x.NetworkReady),
			strconv.FormatBool(x.ApplicationReady), strings.Join(x.Reasons, " | "),
		})
	}
	b, err = csvBytes([]string{"epoch_id", "port", "canary_percent", "duration_seconds", "baseline_connections", "canary_connections",
		"selector_failures", "actual_canary_percent", "allocation_error_pp", "allocation_z", "baseline_retrans_percent",
		"canary_retrans_percent", "retrans_delta_pp", "baseline_mean_rtt_ms", "canary_mean_rtt_ms", "mean_rtt_delta_percent",
		"baseline_goodput_per_member_mbps", "canary_goodput_per_member_mbps", "goodput_per_member_delta_percent",
		"baseline_app_requests", "canary_app_requests", "baseline_app_success_percent", "canary_app_success_percent",
		"app_success_delta_pp", "baseline_app_mean_latency_ms", "canary_app_mean_latency_ms", "app_mean_latency_delta_percent",
		"network_ready", "application_ready", "reasons"}, comparisonRows)
	if err != nil {
		zw.Close()
		return nil, err
	}
	if err = writeFile("comparison.csv", b); err != nil {
		zw.Close()
		return nil, err
	}

	plan, _ := json.MarshalIndent(map[string]any{
		"policy": policy,
		"comparison_semantics": map[string]string{
			"retrans_delta_pp":                 "canary retransmission percent minus baseline, percentage points",
			"mean_rtt_delta_percent":           "relative canary-vs-baseline change; negative is lower RTT",
			"goodput_per_member_delta_percent": "relative canary-vs-baseline change; interpret only when allocation is healthy",
			"app_success_delta_pp":             "canary application success percent minus baseline, percentage points",
			"app_mean_latency_delta_percent":   "relative canary-vs-baseline change; negative is lower latency",
		},
		"statistical_plan": []string{
			"Use only network_ready epochs for network inference and application_ready epochs for application inference.",
			"Never pool epochs with different percentage, rate, gain, or code version.",
			"Use simultaneous within-epoch baseline/canary comparisons to control for time-varying path conditions.",
			"For network time-series metrics, use minute-level paired differences and a time-block bootstrap (recommended block length 5 minutes).",
			"For application success, report cohort rates with Wilson intervals and the difference in rates; if repeated decisions are made, predefine stage windows instead of repeatedly peeking at p-values.",
			"For latency percentiles, ingest application-side histograms in a future schema extension; current schema supports mean and max only.",
			"Treat selector failures, data gaps, and randomization imbalance as validity failures, not algorithm performance.",
		},
	}, "", "  ")
	if err = writeFile("analysis_plan.json", plan); err != nil {
		zw.Close()
		return nil, err
	}

	analysis := map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"analysis_rules": []string{
			"Analyze baseline and canary only within the same epoch.",
			"Exclude or separately flag intervals where gap=true.",
			"Weight RTT means by rtt_samples, not by row count.",
			"Compare retransmission as retrans_bytes/sent_bytes.",
			"Compare throughput using goodput_per_member_mbps when cohort concurrency differs.",
			"Use assigned connection counts to verify actual randomization ratio and selector failures.",
			"Treat application success and latency as preferred decision metrics when available.",
			"Do not pool epochs with different canary percentages, code versions, rate targets, or gain settings.",
		},
	}
	j, _ := json.MarshalIndent(analysis, "", "  ")
	if err = writeFile("analysis_rules.json", j); err != nil {
		zw.Close()
		return nil, err
	}

	cw, err := zw.Create("checksums.sha256")
	if err != nil {
		zw.Close()
		return nil, err
	}
	if _, err = cw.Write([]byte(strings.Join(checksums, "\n") + "\n")); err != nil {
		zw.Close()
		return nil, err
	}

	if err = zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

[executed on device: kr (5928de2c-0d9a-4a94-b93c-5d373f72c8bb)]