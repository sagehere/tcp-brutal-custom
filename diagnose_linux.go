//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

type diagnosticResult struct {
	Available bool   `json:"available"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
}

type diagnosticPort struct {
	portState
	Selector *selectorCount `json:"selector"`
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("diagnostic output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func diagnosticCommand(ctx context.Context, name string, args ...string) diagnosticResult {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	output := &boundedOutput{limit: 64 * 1024}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Run(); err != nil {
		return diagnosticResult{Error: err.Error()}
	}
	return diagnosticResult{Available: true, Output: strings.TrimSpace(output.String())}
}

func (m *manager) diagnose(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	statuses := budgetStatuses(m.cfg)
	m.mu.Unlock()
	states, err := parsePorts()
	stateResult := map[string]any{"available": err == nil, "ports": states}
	if err != nil {
		stateResult["error"] = err.Error()
	}
	ports := []diagnosticPort{}
	for _, state := range states {
		entry := diagnosticPort{portState: state}
		if state.Active && m.selector != nil {
			count, countErr := m.selector.Count(state.Port)
			if countErr == nil {
				entry.Selector = &count
			}
		}
		ports = append(ports, entry)
	}
	stateResult["ports"] = ports
	jsonReply(w, 200, map[string]any{
		"managed_ports": stateResult,
		"bpf_attached":  m.selector != nil && m.selector.link != nil,
		"tcp":           diagnosticCommand(r.Context(), "ss", "-H", "-n", "-t", "-i", "-O"),
		"qdisc":         diagnosticCommand(r.Context(), "tc", "-s", "qdisc", "show"),
		"buffers":       diagnosticCommand(r.Context(), "sysctl", "net.ipv4.tcp_rmem", "net.ipv4.tcp_wmem", "net.ipv4.tcp_limit_output_bytes"),
		"budgets":       statuses,
		"notes":         []string{"Only host passive TCP sending is managed; existing connections are not switched by adding a rule.", "ss fields such as rwnd_limited, sndbuf_limited, app_limited, delivery_rate and pacing_rate are kernel-dependent; missing fields are unavailable, not zero.", "Budgets are advisory; retired groups, destination groups, unassigned ports and other traffic are not included."},
	})
}

func (m *manager) budgets(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Method == "PUT" {
		var next []budgetConfig
		if err := decode(r, &next); err != nil {
			bad(w, 400, err)
			return
		}
		if err := validateBudgets(next, m.cfg.Ports); err != nil {
			bad(w, 400, err)
			return
		}
		c := m.cfg
		c.Budgets = next
		if err := saveConfig(c); err != nil {
			bad(w, 500, err)
			return
		}
		m.cfg = c
		m.history.addEvent("budgets_change", next)
	}
	jsonReply(w, 200, map[string]any{"budgets": m.cfg.Budgets, "status": budgetStatuses(m.cfg), "advisory_only": true, "uncovered": "retired groups, destination groups, unassigned ports and other traffic"})
}
