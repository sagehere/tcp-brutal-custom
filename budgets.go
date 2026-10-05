package main

import (
	"errors"
	"fmt"
	"math"
	"regexp"
)

type budgetConfig struct {
	Name           string   `json:"name"`
	CapacityMbps   float64  `json:"capacity_mbps"`
	ReservePercent *float64 `json:"reserve_percent,omitempty"`
}

type budgetStatus struct {
	Name                 string  `json:"name"`
	CapacityMbps         float64 `json:"capacity_mbps"`
	ReservePercent       float64 `json:"reserve_percent"`
	AvailableMbps        float64 `json:"available_mbps"`
	RequestedMbps        float64 `json:"requested_mbps"`
	ExcessMbps           float64 `json:"excess_mbps"`
	Warning              bool    `json:"warning"`
	ReduceTargetsPercent float64 `json:"reduce_targets_percent"`
}

var budgetName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,48}$`)

func reservePercent(b budgetConfig) float64 {
	if b.ReservePercent == nil {
		return 10
	}
	return *b.ReservePercent
}

func compensationCap(p portConfig) uint32 {
	if p.CompensationCapPercent == 0 {
		return 125
	}
	return p.CompensationCapPercent
}

func validateBudgets(budgets []budgetConfig, ports []portConfig) error {
	names := map[string]bool{}
	for _, b := range budgets {
		r := reservePercent(b)
		if !budgetName.MatchString(b.Name) || names[b.Name] {
			return errors.New("invalid or duplicate budget name")
		}
		if math.IsNaN(b.CapacityMbps) || math.IsInf(b.CapacityMbps, 0) || b.CapacityMbps < 0.5 || b.CapacityMbps > 1000000 || math.IsNaN(r) || math.IsInf(r, 0) || r < 0 || r >= 100 {
			return errors.New("budget capacity or reserve out of range")
		}
		names[b.Name] = true
	}
	seen := map[uint16]bool{}
	for _, p := range ports {
		if seen[p.Port] {
			return fmt.Errorf("duplicate port %d", p.Port)
		}
		seen[p.Port] = true
		if p.Budget != "" && !names[p.Budget] {
			return fmt.Errorf("unknown budget %q", p.Budget)
		}
	}
	return nil
}

func budgetStatuses(c config) []budgetStatus {
	out := make([]budgetStatus, 0, len(c.Budgets))
	for _, b := range c.Budgets {
		r := reservePercent(b)
		s := budgetStatus{Name: b.Name, CapacityMbps: b.CapacityMbps, ReservePercent: r, AvailableMbps: b.CapacityMbps * (1 - r/100)}
		for _, p := range c.Ports {
			if p.Enabled && p.Budget == b.Name {
				s.RequestedMbps += p.RateMbps * float64(compensationCap(p)) / 100
			}
		}
		s.ExcessMbps = math.Max(0, s.RequestedMbps-s.AvailableMbps)
		s.Warning = s.ExcessMbps > 0
		if s.Warning {
			s.ReduceTargetsPercent = 100 * (1 - s.AvailableMbps/s.RequestedMbps)
		}
		out = append(out, s)
	}
	return out
}
