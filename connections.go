//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type connection struct {
	LocalIP    string `json:"local_ip"`
	ClientIP   string `json:"client_ip"`
	ClientPort uint16 `json:"client_port"`
	State      string `json:"state"`
	Algorithm  string `json:"algorithm"`
	Managed    bool   `json:"managed"`
}

func ssEndpoint(value string) (string, uint16, error) {
	i := strings.LastIndexByte(value, ':')
	if i < 0 {
		return "", 0, errors.New("missing endpoint port")
	}
	ip := strings.Trim(value[:i], "[]")
	port, err := strconv.ParseUint(value[i+1:], 10, 16)
	if err != nil || net.ParseIP(strings.SplitN(ip, "%", 2)[0]) == nil {
		return "", 0, errors.New("invalid endpoint")
	}
	return ip, uint16(port), nil
}

func parseConnections(output []byte, port uint16) ([]connection, error) {
	rows := []connection{}
	s := bufio.NewScanner(bytes.NewReader(output))
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 5 {
			return nil, fmt.Errorf("unexpected ss output: %q", s.Text())
		}
		localIP, localPort, err := ssEndpoint(fields[3])
		if err != nil {
			return nil, err
		}
		if localPort != port {
			continue
		}
		clientIP, clientPort, err := ssEndpoint(fields[4])
		if err != nil {
			return nil, err
		}
		if fields[0] == "LISTEN" || fields[0] == "TIME-WAIT" {
			continue
		}
		algorithm := "unknown"
		if len(fields) > 5 && !strings.Contains(fields[5], ":") {
			algorithm = fields[5]
		}
		rows = append(rows, connection{LocalIP: localIP, ClientIP: clientIP, ClientPort: clientPort, State: fields[0], Algorithm: algorithm, Managed: algorithm == "brutal_adaptive"})
	}
	return rows, s.Err()
}

func (m *manager) connections(w http.ResponseWriter, r *http.Request) {
	value, err := strconv.ParseUint(r.URL.Query().Get("port"), 10, 16)
	if err != nil || value == 0 {
		bad(w, 400, errors.New("invalid port"))
		return
	}
	port := uint16(value)
	m.mu.Lock()
	allowed := false
	for _, p := range m.cfg.Ports {
		allowed = allowed || p.Port == port
	}
	m.mu.Unlock()
	if !allowed {
		states, e := parsePorts()
		if e != nil {
			bad(w, 503, e)
			return
		}
		for _, p := range states {
			allowed = allowed || (p.Port == port && p.Members > 0)
		}
	}
	if !allowed {
		bad(w, 404, errors.New("port is not managed"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ss", "-H", "-n", "-t", "-i", "-O", "state", "connected", "(", "sport", "=", ":"+strconv.Itoa(int(port)), ")").Output()
	if err != nil {
		bad(w, 503, fmt.Errorf("connection lookup: %w", err))
		return
	}
	if len(out) > 1<<20 {
		bad(w, 503, errors.New("too many connections"))
		return
	}
	rows, err := parseConnections(out, port)
	if err != nil {
		bad(w, 503, err)
		return
	}
	jsonReply(w, 200, map[string]any{"port": port, "connections": rows})
}
