//go:build linux

package main

import (
	"context"
	"crypto/subtle"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/sys/unix"
)

type portState struct {
	Port       uint16        `json:"port"`
	Active     bool          `json:"active"`
	Rate       uint64        `json:"rate_bytes_per_second"`
	Gain       uint32        `json:"gain"`
	Group      uint64        `json:"group"`
	Members    uint32        `json:"members"`
	Sent       uint64        `json:"sent"`
	Acked      uint64        `json:"acked"`
	Retrans    uint64        `json:"retrans"`
	Expected   uint64        `json:"expected_bytes"`
	Actual     uint64        `json:"actual_bytes"`
	RTTSum     uint64        `json:"rtt_sum_us"`
	RTTSamples uint64        `json:"rtt_samples"`
	RTTMax     uint32        `json:"rtt_max_us"`
	Selector   selectorCount `json:"selector"`
}

type session struct {
	csrf    string
	expires time.Time
}
type attempt struct {
	count int
	since time.Time
}
type manager struct {
	mu       sync.Mutex
	cfg      config
	selector *selector
	history  *history
	sessions map[string]session
	attempts map[string]attempt
	job      updateJob
}

type peerKey struct{}

func peerUID(c net.Conn) uint32 {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ^uint32(0)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return ^uint32(0)
	}
	var uid uint32 = ^uint32(0)
	raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if e == nil {
			uid = cred.Uid
		}
	})
	return uid
}

func managerMode() error {
	if os.Geteuid() != 0 {
		return errors.New("root required")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if _, err = os.Stat(portsPath); err != nil {
		return fmt.Errorf("brutal module is not loaded: %w", err)
	}
	s, err := newSelector()
	if err != nil {
		return err
	}
	defer s.Close()
	h, err := openHistory()
	if err != nil {
		return err
	}
	defer h.close()
	m := &manager{cfg: cfg, selector: s, history: h, sessions: map[string]session{}, attempts: map[string]attempt{}}
	for _, p := range cfg.Ports {
		if p.Enabled {
			if err = m.applyPort(p); err != nil {
				return fmt.Errorf("restore port %d: %w", p.Port, err)
			}
		}
	}
	for _, p := range cfg.ABPorts {
		if p.Enabled {
			if err = m.applyABPort(p); err != nil {
				return fmt.Errorf("restore A/B port %d: %w", p.Port, err)
			}
			epochID, e := m.history.ensureABEpoch(p, "manager_restart")
			if e != nil {
				return fmt.Errorf("restore A/B epoch %d: %w", p.Port, e)
			}
			if err = m.history.reconcileABRollout(p, epochID); err != nil {
				return fmt.Errorf("restore A/B rollout %d: %w", p.Port, err)
			}
			if err = m.seedABNow(p); err != nil {
				return fmt.Errorf("restore A/B checkpoint %d: %w", p.Port, err)
			}
		}
	}
	if err = os.MkdirAll("/run/tcp-brutal-canary", 0750); err != nil {
		return err
	}
	group, err := user.LookupGroup("tcpbrutal")
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return err
	}
	os.Chown("/run/tcp-brutal-canary", 0, gid)
	if err = writePublicWebConfig(cfg, gid); err != nil {
		return err
	}
	os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer os.Remove(socketPath)
	defer ln.Close()
	if err = os.Chown(socketPath, 0, gid); err != nil {
		return err
	}
	if err = os.Chmod(socketPath, 0660); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go waitSignal(cancel)
	go m.collect(ctx)
	server := &http.Server{Handler: m.routes(), ReadHeaderTimeout: 5 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, peerKey{}, peerUID(c))
		}}
	go func() { <-ctx.Done(); server.Shutdown(context.Background()) }()
	err = server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func parsePortsAt(path string) ([]portState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := []portState{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		fields := map[string]string{}
		for _, part := range strings.Fields(line) {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) == 2 {
				fields[kv[0]] = kv[1]
			}
		}
		num := func(k string) uint64 { v, _ := strconv.ParseUint(fields[k], 10, 64); return v }
		p := portState{Port: uint16(num("port")), Active: num("active") == 1, Rate: num("rate"), Gain: uint32(num("gain")), Group: num("id"), Members: uint32(num("members")), Sent: num("sent"), Acked: num("acked"), Retrans: num("retrans"), RTTSum: num("rtt_sum"), RTTSamples: num("rtt_samples"), RTTMax: uint32(num("rtt_max"))}
		p.Expected, p.Actual = sendBytes(p.Sent, p.Retrans)
		out = append(out, p)
	}
	return out, nil
}

func parsePorts() ([]portState, error) { return parsePortsAt(portsPath) }

func findPortState(states []portState, port uint16) (portState, bool) {
	var fallback portState
	found := false
	for _, p := range states {
		if p.Port != port {
			continue
		}
		if p.Active {
			return p, true
		}
		if !found {
			fallback = p
			found = true
		}
	}
	return fallback, found
}

func abCohortStates(cfg abPortConfig, baselineStates, canaryStates []portState) (portState, portState, error) {
	base, bok := findPortState(baselineStates, cfg.Port)
	canary, cok := findPortState(canaryStates, cfg.Port)
	if !bok && cfg.CanaryPercent == 100 {
		base, bok = portState{Port: cfg.Port}, true
	}
	if !cok && cfg.CanaryPercent == 0 {
		canary, cok = portState{Port: cfg.Port}, true
	}
	if !bok || !cok {
		return portState{}, portState{}, fmt.Errorf("missing A/B cohort state baseline=%v canary=%v", bok, cok)
	}
	base.Port, canary.Port = cfg.Port, cfg.Port
	return base, canary, nil
}

func writePort(command string) error {
	f, err := os.OpenFile(portsPath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(command + "\n")
	return err
}

func deletePort(writer func(string) error, port uint16) error {
	err := writer(fmt.Sprintf("del %d", port))
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

func validatePort(p portConfig, webPort uint16) error {
	if p.Port == 0 || p.Port == webPort {
		return errors.New("invalid or panel port")
	}
	if p.RateMbps < 0.5 || p.RateMbps > 1000000 || p.Gain < 5 || p.Gain > 80 {
		return errors.New("rate or gain out of range")
	}
	return nil
}

func baselineOwnsPort(port uint16) bool {
	b, err := os.ReadFile(baselinePortsPath)
	if err != nil {
		return false
	}
	needle := fmt.Sprintf("port=%d ", port)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, needle) && strings.Contains(line, " active=1 ") {
			return true
		}
	}
	return false
}

func baselineManagerOwnsPort(port uint16) bool {
	b, err := os.ReadFile(baselineConfigPath)
	if err != nil {
		return false
	}
	var cfg struct {
		Ports []portConfig `json:"ports"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return true
	}
	for _, p := range cfg.Ports {
		if p.Port == port && p.Enabled {
			return true
		}
	}
	return false
}

func writeBaselinePort(command string) error {
	f, err := os.OpenFile(baselinePortsPath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(command + "\n")
	return err
}

func (m *manager) applyABPort(p abPortConfig) error {
	if err := validatePort(portConfig{Port: p.Port, RateMbps: p.RateMbps, Gain: p.Gain, Enabled: p.Enabled}, m.cfg.WebPort); err != nil {
		return err
	}
	if p.CanaryPercent > 100 {
		return errors.New("canary percentage must be between 0 and 100")
	}
	if baselineManagerOwnsPort(p.Port) {
		return fmt.Errorf("port %d is configured in baseline manager; remove it there before A/B takeover", p.Port)
	}
	for _, cp := range m.cfg.Ports {
		if cp.Port == p.Port && cp.Enabled {
			return fmt.Errorf("port %d is already configured as canary-only", p.Port)
		}
	}
	totalRate := uint64(p.RateMbps*1e6/8 + 0.5)
	canaryRate := totalRate * uint64(p.CanaryPercent) / 100
	baselineRate := totalRate - canaryRate
	minRate := uint64(500000 / 8) // 0.5 Mbps, matches kernel MIN_PACING_RATE.
	if p.CanaryPercent > 0 && canaryRate < minRate {
		return fmt.Errorf("canary share is below the 0.5 Mbps kernel minimum; increase total rate or canary percentage")
	}
	if p.CanaryPercent < 100 && baselineRate < minRate {
		return fmt.Errorf("baseline share is below the 0.5 Mbps kernel minimum; increase total rate or reduce canary percentage")
	}

	if baselineRate > 0 {
		if err := writeBaselinePort(fmt.Sprintf("add %d rate=%d gain=%d", p.Port, baselineRate, p.Gain)); err != nil {
			return fmt.Errorf("baseline group: %w", err)
		}
	} else if err := deletePort(writeBaselinePort, p.Port); err != nil {
		return fmt.Errorf("disable baseline group: %w", err)
	}
	if canaryRate > 0 {
		if err := writePort(fmt.Sprintf("add %d rate=%d gain=%d", p.Port, canaryRate, p.Gain)); err != nil {
			_ = deletePort(writeBaselinePort, p.Port)
			return fmt.Errorf("canary group: %w", err)
		}
	} else if err := deletePort(writePort, p.Port); err != nil {
		return fmt.Errorf("disable canary group: %w", err)
	}
	if err := m.selector.Enable(p.Port, p.CanaryPercent); err != nil {
		_ = deletePort(writePort, p.Port)
		_ = deletePort(writeBaselinePort, p.Port)
		return err
	}
	return nil
}

func (m *manager) seedABNow(cfg abPortConfig) error {
	baselineStates, err := parsePortsAt(baselinePortsPath)
	if err != nil {
		return err
	}
	canaryStates, err := parsePortsAt(portsPath)
	if err != nil {
		return err
	}
	base, canary, err := abCohortStates(cfg, baselineStates, canaryStates)
	if err != nil {
		return err
	}
	c, err := m.selector.Count(cfg.Port)
	if err != nil {
		return err
	}
	return m.history.seedAB(cfg.Port, base, canary, c)
}

func (m *manager) disableABPort(port uint16) error {
	if err := m.selector.Disable(port); err != nil {
		return err
	}
	var first error
	if err := deletePort(writePort, port); err != nil {
		first = err
	}
	if err := deletePort(writeBaselinePort, port); err != nil && first == nil {
		first = err
	}
	return first
}

func (m *manager) applyPort(p portConfig) error {
	if err := validatePort(p, m.cfg.WebPort); err != nil {
		return err
	}
	if baselineOwnsPort(p.Port) {
		return fmt.Errorf("port %d is already managed by baseline tcp-brutal-custom", p.Port)
	}
	rate := uint64(p.RateMbps*1e6/8 + 0.5)
	if err := writePort(fmt.Sprintf("add %d rate=%d gain=%d", p.Port, rate, p.Gain)); err != nil {
		return err
	}
	if err := m.selector.Enable(p.Port, 100); err != nil {
		return err
	}
	return nil
}

func (m *manager) disablePort(port uint16) error {
	if err := m.selector.Disable(port); err != nil {
		return err
	}
	if err := deletePort(writePort, port); err != nil {
		m.selector.Enable(port, 100)
		return err
	}
	return nil
}

func (m *manager) collect(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	cleanup := time.NewTicker(time.Hour)
	defer cleanup.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			canaryStates, err := parsePortsAt(portsPath)
			if err != nil {
				log.Printf("sampling canary: %v", err)
				continue
			}
			baselineStates, baselineErr := parsePortsAt(baselinePortsPath)
			now := time.Now().Unix()

			// Preserve the existing canary-only history.
			for _, p := range canaryStates {
				var c selectorCount
				if p.Active {
					c, err = m.selector.Count(p.Port)
					if err != nil {
						log.Printf("selector count: %v", err)
						continue
					}
				}
				x := sample{Time: now, Port: p.Port, Group: p.Group, Sent: p.Sent, Acked: p.Acked, Retrans: p.Retrans, Success: c.Success(), Failure: c.Failure, Members: p.Members, RTTSum: p.RTTSum, RTTSamples: p.RTTSamples, RTTMax: p.RTTMax}
				if err = m.history.record(x); err != nil {
					log.Printf("history: %v", err)
				}
			}

			if baselineErr != nil {
				log.Printf("sampling baseline: %v", baselineErr)
				continue
			}
			m.mu.Lock()
			abPorts := append([]abPortConfig(nil), m.cfg.ABPorts...)
			m.mu.Unlock()
			for _, cfg := range abPorts {
				if !cfg.Enabled {
					continue
				}
				epochID := m.history.currentABEpoch(cfg.Port)
				if epochID == 0 {
					epochID, err = m.history.ensureABEpoch(cfg, "collector_recovery")
					if err != nil {
						log.Printf("A/B epoch %d: %v", cfg.Port, err)
						continue
					}
				}
				base, canary, stateErr := abCohortStates(cfg, baselineStates, canaryStates)
				if stateErr != nil {
					log.Printf("A/B sample port %d: %v", cfg.Port, stateErr)
					continue
				}
				c, e := m.selector.Count(cfg.Port)
				if e != nil {
					log.Printf("A/B selector %d: %v", cfg.Port, e)
					continue
				}
				if err = m.history.recordABCohort(epochID, "baseline", base, now); err != nil {
					log.Printf("A/B baseline history %d: %v", cfg.Port, err)
				}
				if err = m.history.recordABCohort(epochID, "canary", canary, now); err != nil {
					log.Printf("A/B canary history %d: %v", cfg.Port, err)
				}
				if err = m.history.recordABSelector(epochID, cfg.Port, c, now); err != nil {
					log.Printf("A/B selector history %d: %v", cfg.Port, err)
				}
				if cfg.SafetyPlan != nil {
					eval, safetyErr := m.evaluateABSafety(cfg, epochID, now)
					if safetyErr != nil {
						log.Printf("A/B safety evaluation %d: %v", cfg.Port, safetyErr)
					} else {
						rolloutID := int64(0)
						if rollout, e := m.history.activeABRollout(cfg.Port); e == nil && rollout != nil {
							rolloutID = rollout.ID
						}
						if e := m.history.syncABSafetyAlerts(eval, rolloutID); e != nil {
							log.Printf("A/B safety alerts %d: %v", cfg.Port, e)
						}
					}
				}
			}
		case <-cleanup.C:
			if err := m.history.prune(); err != nil {
				log.Printf("retention: %v", err)
			}
		}
	}
}

func jsonReply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func bad(w http.ResponseWriter, status int, err error) {
	jsonReply(w, status, map[string]string{"error": err.Error()})
}
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, 65536))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

func (m *manager) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Context().Value(peerKey{}) == uint32(0) {
		return true
	}
	cookie, err := r.Cookie("session")
	if err != nil {
		bad(w, 401, errors.New("login required"))
		return false
	}
	m.mu.Lock()
	s, ok := m.sessions[cookie.Value]
	m.mu.Unlock()
	if !ok || time.Now().After(s.expires) {
		bad(w, 401, errors.New("session expired"))
		return false
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-CSRF-Token") != s.csrf {
		bad(w, 403, errors.New("CSRF token required"))
		return false
	}
	return true
}

func (m *manager) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/login", m.login)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'")
		w.Write(panelHTML)
	})
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(panelJS)
	})
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		if !m.authorized(w, r) {
			return
		}
		m.api(w, r)
	})
	return mux
}

func (m *manager) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, 400, err)
		return
	}
	ip := r.Header.Get("X-Client-IP")
	if net.ParseIP(ip) == nil {
		ip = "local"
	}
	m.mu.Lock()
	a := m.attempts[ip]
	if time.Since(a.since) > time.Minute {
		a = attempt{since: time.Now()}
	}
	if a.count >= 5 {
		m.mu.Unlock()
		bad(w, 429, errors.New("too many attempts"))
		return
	}
	a.count++
	m.attempts[ip] = a
	cfg := m.cfg
	m.mu.Unlock()
	salt, err := hex.DecodeString(cfg.PasswordSalt)
	if err != nil {
		bad(w, 500, err)
		return
	}
	hash, err := hex.DecodeString(cfg.PasswordHash)
	if err != nil {
		bad(w, 500, err)
		return
	}
	candidate := argon2.IDKey([]byte(in.Password), salt, 3, 64*1024, 4, 32)
	if subtle.ConstantTimeCompare(candidate, hash) != 1 {
		bad(w, 401, errors.New("invalid password"))
		return
	}
	token, err := randomToken(32)
	if err != nil {
		bad(w, 500, err)
		return
	}
	csrf, err := randomToken(16)
	if err != nil {
		bad(w, 500, err)
		return
	}
	m.mu.Lock()
	for key, s := range m.sessions {
		if time.Now().After(s.expires) {
			delete(m.sessions, key)
		}
	}
	for key, a := range m.attempts {
		if time.Since(a.since) > time.Minute {
			delete(m.attempts, key)
		}
	}
	if len(m.sessions) >= 1024 {
		m.mu.Unlock()
		bad(w, 503, errors.New("session capacity reached"))
		return
	}
	delete(m.attempts, ip)
	m.sessions[token] = session{csrf: csrf, expires: time.Now().Add(8 * time.Hour)}
	m.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 3600})
	jsonReply(w, 200, map[string]string{"csrf": csrf})
}

func (m *manager) api(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v1/status":
		states, err := parsePorts()
		if err != nil {
			bad(w, 503, err)
			return
		}
		for i := range states {
			if states[i].Active {
				states[i].Selector, _ = m.selector.Count(states[i].Port)
			}
		}
		m.mu.Lock()
		cfg := m.cfg
		job := m.job
		m.mu.Unlock()
		if job.ID == "" {
			if b, e := os.ReadFile(dataDir + "/update-id"); e == nil {
				job.ID = strings.TrimSpace(string(b))
			}
		}
		var saved struct {
			State  string `json:"state"`
			Detail string `json:"detail"`
			Time   int64  `json:"time"`
		}
		if b, e := os.ReadFile(dataDir + "/update.json"); e == nil && json.Unmarshal(b, &saved) == nil && saved.Time >= job.Started {
			job.State = saved.State
			job.Detail = saved.Detail
		}
		jsonReply(w, 200, map[string]any{"version": version, "ports": states, "configured_ports": cfg.Ports, "web_host": cfg.WebHost, "web_port": cfg.WebPort, "allowed_ips": cfg.AllowedIPs, "bpf_attached": m.selector.link != nil, "update": job})
	case r.Method == "GET" && r.URL.Path == "/api/v1/ports":
		states, err := parsePorts()
		if err != nil {
			bad(w, 503, err)
			return
		}
		for i := range states {
			if states[i].Active {
				states[i].Selector, _ = m.selector.Count(states[i].Port)
			}
		}
		jsonReply(w, 200, states)
	case r.Method == "GET" && r.URL.Path == "/api/v1/connections":
		m.connections(w, r)
	case r.Method == "POST" && r.URL.Path == "/api/v1/ports":
		m.putPort(w, r)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/v1/ports/"):
		m.deletePort(w, r)
	case r.Method == "GET" && r.URL.Path == "/api/v1/ab":
		m.listAB(w, r)
	case r.Method == "POST" && r.URL.Path == "/api/v1/ab":
		m.putAB(w, r)
	case (r.Method == "PUT" || r.Method == "DELETE") && strings.HasPrefix(r.URL.Path, "/api/v1/ab/"):
		m.changeAB(w, r)
	case r.Method == "GET" && r.URL.Path == "/api/v1/ab/ports":
		m.abPortsAPI(w, r)
	case r.Method == "GET" && r.URL.Path == "/api/v1/ab/summary":
		m.abSummaryAPI(w, r)
	case r.Method == "GET" && r.URL.Path == "/api/v1/ab/analysis":
		m.abAnalysisAPI(w, r)
	case r.Method == "GET" && r.URL.Path == "/api/v1/ab/safety":
		m.abSafetyAPI(w, r)
	case r.Method == "GET" && r.URL.Path == "/api/v1/ab/rollout":
		m.abRolloutAPI(w, r)
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v1/ab/rollout/"):
		m.abRolloutActionAPI(w, r)
	case r.Method == "GET" && r.URL.Path == "/api/v1/ab/series":
		m.abSeriesAPI(w, r)
	case r.Method == "GET" && r.URL.Path == "/api/v1/ab/report":
		m.abReportAPI(w, r)
	case r.Method == "POST" && r.URL.Path == "/api/v1/ab/app-metrics":
		m.abAppMetricsAPI(w, r)
	case r.Method == "GET" && r.URL.Path == "/api/v1/metrics":
		m.metrics(w, r)
	case r.Method == "PUT" && r.URL.Path == "/api/v1/password":
		m.changePassword(w, r)
	case r.Method == "POST" && r.URL.Path == "/api/v1/password/reset":
		m.resetPassword(w, r)
	case r.Method == "PUT" && r.URL.Path == "/api/v1/settings":
		m.settings(w, r)
	case r.Method == "PUT" && r.URL.Path == "/api/v1/autostart":
		m.autostart(w, r)
	case r.Method == "GET" && r.URL.Path == "/api/v1/autostart":
		m.autostartStatus(w, r)
	case r.Method == "GET" && r.URL.Path == "/api/v1/update/check":
		m.checkUpdate(w, r)
	case r.Method == "POST" && r.URL.Path == "/api/v1/update":
		m.startUpdate(w, r)
	default:
		bad(w, 404, errors.New("unknown API"))
	}
}

func (m *manager) putPort(w http.ResponseWriter, r *http.Request) {
	var p portConfig
	if err := decode(r, &p); err != nil {
		bad(w, 400, err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validatePort(p, m.cfg.WebPort); err != nil {
		bad(w, 400, err)
		return
	}
	idx := -1
	for i, v := range m.cfg.Ports {
		if v.Port == p.Port {
			idx = i
			break
		}
	}
	var previous portConfig
	if idx >= 0 {
		previous = m.cfg.Ports[idx]
	}
	if p.Enabled {
		if err := m.applyPort(p); err != nil {
			if idx >= 0 && previous.Enabled {
				m.applyPort(previous)
			} else {
				writePort(fmt.Sprintf("del %d", p.Port))
			}
			bad(w, 500, err)
			return
		}
	} else if idx >= 0 && previous.Enabled {
		if err := m.disablePort(p.Port); err != nil {
			bad(w, 500, err)
			return
		}
	}
	next := m.cfg
	next.Ports = append([]portConfig(nil), m.cfg.Ports...)
	if idx >= 0 {
		next.Ports[idx] = p
	} else {
		next.Ports = append(next.Ports, p)
	}
	if err := saveConfig(next); err != nil {
		if idx >= 0 && previous.Enabled {
			m.applyPort(previous)
		} else if p.Enabled {
			m.disablePort(p.Port)
		}
		bad(w, 500, err)
		return
	}
	m.cfg = next
	m.history.addEvent("port_change", map[string]any{"before": previous, "after": p})
	jsonReply(w, 200, p)
}

func (m *manager) deletePort(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseUint(strings.TrimPrefix(r.URL.Path, "/api/v1/ports/"), 10, 16)
	if err != nil || n == 0 {
		bad(w, 400, errors.New("invalid port"))
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := -1
	for i, p := range m.cfg.Ports {
		if p.Port == uint16(n) {
			idx = i
			break
		}
	}
	if idx < 0 {
		bad(w, 404, errors.New("port not configured"))
		return
	}
	previous := m.cfg.Ports[idx]
	if previous.Enabled {
		if err = m.disablePort(previous.Port); err != nil {
			bad(w, 500, err)
			return
		}
	}
	next := m.cfg
	next.Ports = append(append([]portConfig(nil), m.cfg.Ports[:idx]...), m.cfg.Ports[idx+1:]...)
	if err = saveConfig(next); err != nil {
		if previous.Enabled {
			m.applyPort(previous)
		}
		bad(w, 500, err)
		return
	}
	m.cfg = next
	m.history.addEvent("port_delete", previous)
	jsonReply(w, 200, map[string]any{"deleted": n})
}

func (m *manager) listAB(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	ports := append([]abPortConfig(nil), m.cfg.ABPorts...)
	m.mu.Unlock()
	type row struct {
		abPortConfig
		Selector selectorCount `json:"selector"`
	}
	out := make([]row, 0, len(ports))
	for _, p := range ports {
		var c selectorCount
		if p.Enabled {
			c, _ = m.selector.Count(p.Port)
		}
		out = append(out, row{abPortConfig: p, Selector: c})
	}
	jsonReply(w, 200, out)
}

func (m *manager) putAB(w http.ResponseWriter, r *http.Request) {
	var p abPortConfig
	if err := decode(r, &p); err != nil {
		bad(w, 400, err)
		return
	}
	if p.Gain == 0 {
		p.Gain = 20
	}
	plan, err := effectiveABExperimentPlan(p.AnalysisPlan)
	if err != nil {
		bad(w, 400, err)
		return
	}
	p.AnalysisPlan = &plan
	rollout, err := effectiveABRolloutPlan(p.RolloutPlan, p.CanaryPercent)
	if err != nil {
		bad(w, 400, err)
		return
	}
	if rollout != nil && rollout.Stages[0] != p.CanaryPercent {
		bad(w, 400, errors.New("first rollout stage must equal the initial canary percentage"))
		return
	}
	p.RolloutPlan = rollout
	safety, err := effectiveABSafetyPlan(p.SafetyPlan)
	if err != nil {
		bad(w, 400, err)
		return
	}
	p.SafetyPlan = &safety
	p.Enabled = true
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.cfg.ABPorts {
		if v.Port == p.Port {
			bad(w, 409, errors.New("A/B port already configured"))
			return
		}
	}
	if err := m.applyABPort(p); err != nil {
		bad(w, 500, err)
		return
	}
	previousCfg := m.cfg
	next := previousCfg
	next.ABPorts = append(append([]abPortConfig(nil), previousCfg.ABPorts...), p)
	if err := saveConfig(next); err != nil {
		_ = m.disableABPort(p.Port)
		bad(w, 500, err)
		return
	}
	m.cfg = next
	epochID, err := m.history.beginABEpoch(p, "ab_add")
	if err != nil {
		_ = m.disableABPort(p.Port)
		_ = saveConfig(previousCfg)
		m.cfg = previousCfg
		bad(w, 500, err)
		return
	}
	if p.RolloutPlan != nil {
		if _, err = m.history.beginABRollout(p, epochID); err != nil {
			_ = m.history.closeABEpoch(p.Port, "rollout_create_failed")
			_ = m.disableABPort(p.Port)
			_ = saveConfig(previousCfg)
			m.cfg = previousCfg
			bad(w, 500, err)
			return
		}
	}
	if err := m.seedABNow(p); err != nil {
		log.Printf("A/B epoch seed %d: %v", p.Port, err)
	}
	m.history.addEvent("ab_add", p)
	jsonReply(w, 200, p)
}

func (m *manager) changeAB(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseUint(strings.TrimPrefix(r.URL.Path, "/api/v1/ab/"), 10, 16)
	if err != nil || n == 0 {
		bad(w, 400, errors.New("invalid port"))
		return
	}
	port := uint16(n)
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := -1
	for i, p := range m.cfg.ABPorts {
		if p.Port == port {
			idx = i
			break
		}
	}
	if idx < 0 {
		bad(w, 404, errors.New("A/B port not configured"))
		return
	}
	previous := m.cfg.ABPorts[idx]
	if r.Method == "DELETE" {
		if previous.Enabled {
			if err = m.disableABPort(port); err != nil {
				bad(w, 500, err)
				return
			}
		}
		next := m.cfg
		next.ABPorts = append(append([]abPortConfig(nil), m.cfg.ABPorts[:idx]...), m.cfg.ABPorts[idx+1:]...)
		if err = saveConfig(next); err != nil {
			_ = m.applyABPort(previous)
			bad(w, 500, err)
			return
		}
		m.cfg = next
		if err := m.history.closeABEpoch(port, "ab_delete"); err != nil {
			log.Printf("A/B epoch close %d: %v", port, err)
		}
		if rollout, e := m.history.activeABRollout(port); e == nil && rollout != nil {
			_ = m.history.closeABRolloutStage(rollout.CurrentStageID, "stopped", "ab_delete")
			_ = m.history.setABRolloutStatus(rollout.ID, "stopped", true)
			_ = m.history.addABRolloutEvent(rollout.ID, port, "stop", previous.CanaryPercent, previous.CanaryPercent, rollout.CurrentEpochID, "A/B experiment deleted")
		}
		m.history.addEvent("ab_delete", previous)
		jsonReply(w, 200, map[string]any{"deleted": port})
		return
	}
	var in struct {
		CanaryPercent uint8 `json:"canary_percent"`
	}
	if err = decode(r, &in); err != nil || in.CanaryPercent > 100 {
		bad(w, 400, errors.New("invalid canary percentage"))
		return
	}
	if previous.RolloutPlan != nil {
		bad(w, 409, errors.New("managed rollout percentage cannot be changed directly; use rollout actions"))
		return
	}
	next := m.cfg
	next.ABPorts = append([]abPortConfig(nil), m.cfg.ABPorts...)
	next.ABPorts[idx].CanaryPercent = in.CanaryPercent
