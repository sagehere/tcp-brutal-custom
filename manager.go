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
			if _, err = m.history.ensureABEpoch(p, "manager_restart"); err != nil {
				return fmt.Errorf("restore A/B epoch %d: %w", p.Port, err)
			}
			if err = m.seedABNow(p.Port); err != nil {
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
	for _, p := range states {
		if p.Port == port {
			return p, true
		}
	}
	return portState{}, false
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
	} else if err := writeBaselinePort(fmt.Sprintf("del %d", p.Port)); err != nil {
		return fmt.Errorf("disable baseline group: %w", err)
	}
	if canaryRate > 0 {
		if err := writePort(fmt.Sprintf("add %d rate=%d gain=%d", p.Port, canaryRate, p.Gain)); err != nil {
			_ = writeBaselinePort(fmt.Sprintf("del %d", p.Port))
			return fmt.Errorf("canary group: %w", err)
		}
	} else if err := writePort(fmt.Sprintf("del %d", p.Port)); err != nil {
		return fmt.Errorf("disable canary group: %w", err)
	}
	if err := m.selector.Enable(p.Port, p.CanaryPercent); err != nil {
		_ = writePort(fmt.Sprintf("del %d", p.Port))
		_ = writeBaselinePort(fmt.Sprintf("del %d", p.Port))
		return err
	}
	return nil
}

func (m *manager) seedABNow(port uint16) error {
	baselineStates, err := parsePortsAt(baselinePortsPath)
	if err != nil {
		return err
	}
	canaryStates, err := parsePortsAt(portsPath)
	if err != nil {
		return err
	}
	base, bok := findPortState(baselineStates, port)
	canary, cok := findPortState(canaryStates, port)
	if !bok || !cok {
		return fmt.Errorf("missing A/B cohort state baseline=%v canary=%v", bok, cok)
	}
	c, err := m.selector.Count(port)
	if err != nil {
		return err
	}
	base.Port, canary.Port = port, port
	return m.history.seedAB(port, base, canary, c)
}

func (m *manager) disableABPort(port uint16) error {
	if err := m.selector.Disable(port); err != nil {
		return err
	}
	var first error
	if err := writePort(fmt.Sprintf("del %d", port)); err != nil {
		first = err
	}
	if err := writeBaselinePort(fmt.Sprintf("del %d", port)); err != nil && first == nil {
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
	if err := writePort(fmt.Sprintf("del %d", port)); err != nil {
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
				base, bok := findPortState(baselineStates, cfg.Port)
				canary, cok := findPortState(canaryStates, cfg.Port)
				if !bok || !cok {
					log.Printf("A/B sample port %d missing cohort state baseline=%v canary=%v", cfg.Port, bok, cok)
					continue
				}
				base.Port, canary.Port = cfg.Port, cfg.Port
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
	case r.Method == "GET" && r.URL.Path == "/api/v1/ab/summary":
		m.abSummaryAPI(w, r)
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
	next := m.cfg
	next.ABPorts = append(append([]abPortConfig(nil), m.cfg.ABPorts...), p)
	if err := saveConfig(next); err != nil {
		_ = m.disableABPort(p.Port)
		bad(w, 500, err)
		return
	}
	m.cfg = next
	if _, err := m.history.beginABEpoch(p, "ab_add"); err != nil {
		log.Printf("A/B epoch start %d: %v", p.Port, err)
	} else if err := m.seedABNow(p.Port); err != nil {
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
	next := m.cfg
	next.ABPorts = append([]abPortConfig(nil), m.cfg.ABPorts...)
	next.ABPorts[idx].CanaryPercent = in.CanaryPercent
	if err = m.applyABPort(next.ABPorts[idx]); err != nil {
		bad(w, 500, err)
		return
	}
	if err = saveConfig(next); err != nil {
		_ = m.applyABPort(previous)
		bad(w, 500, err)
		return
	}
	m.cfg = next
	if _, err := m.history.beginABEpoch(next.ABPorts[idx], "percentage_change"); err != nil {
		log.Printf("A/B epoch change %d: %v", port, err)
	} else if err := m.seedABNow(port); err != nil {
		log.Printf("A/B epoch seed %d: %v", port, err)
	}
	m.history.addEvent("ab_percent", map[string]any{"port": port, "before": previous.CanaryPercent, "after": in.CanaryPercent})
	jsonReply(w, 200, next.ABPorts[idx])
}

func abRange(r *http.Request) (uint16, int64, int64, error) {
	q := r.URL.Query()
	pv, err := strconv.ParseUint(q.Get("port"), 10, 16)
	if err != nil || pv == 0 {
		return 0, 0, 0, errors.New("valid port is required")
	}
	to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
	if to == 0 {
		to = time.Now().Unix()
	}
	from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	if from == 0 {
		from = to - 24*3600
	}
	if from >= to || to-from > 366*86400 {
		return 0, 0, 0, errors.New("invalid time range")
	}
	return uint16(pv), from, to, nil
}

func (m *manager) abSummaryAPI(w http.ResponseWriter, r *http.Request) {
	port, from, to, err := abRange(r)
	if err != nil {
		bad(w, 400, err)
		return
	}
	rows, err := m.history.abSummaries(port, from, to)
	if err != nil {
		bad(w, 500, err)
		return
	}
	epochs, err := m.history.abEpochs(port, from, to)
	if err != nil {
		bad(w, 500, err)
		return
	}
	jsonReply(w, 200, map[string]any{"port": port, "from": from, "to": to, "epochs": epochs, "summaries": rows})
}

func (m *manager) abReportAPI(w http.ResponseWriter, r *http.Request) {
	port, from, to, err := abRange(r)
	if err != nil {
		bad(w, 400, err)
		return
	}
	tier := r.URL.Query().Get("tier")
	if tier == "" {
		tier = "minute"
	}
	data, err := buildABReport(m.history, port, from, to, tier)
	if err != nil {
		bad(w, 500, err)
		return
	}
	name := fmt.Sprintf("brutal-ab-port-%d-%d-%d.zip", port, from, to)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (m *manager) abAppMetricsAPI(w http.ResponseWriter, r *http.Request) {
	var x abAppSample
	if err := decode(r, &x); err != nil {
		bad(w, 400, err)
		return
	}
	if x.Port == 0 || (x.Cohort != "baseline" && x.Cohort != "canary") {
		bad(w, 400, errors.New("port and cohort are required"))
		return
	}
	if x.Requests != x.Success+x.Errors {
		bad(w, 400, errors.New("requests must equal success + errors"))
		return
	}
	if x.LatencySamples > x.Requests {
		bad(w, 400, errors.New("latency_samples cannot exceed requests"))
		return
	}
	if err := m.history.recordABApp(x); err != nil {
		bad(w, 500, err)
		return
	}
	jsonReply(w, 200, map[string]any{"stored": true, "port": x.Port, "cohort": x.Cohort, "source": x.Source})
}

func (m *manager) metrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tier := q.Get("tier")
	if tier == "" {
		tier = "minute"
	}
	from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
	if to == 0 {
		to = time.Now().Unix()
	}
	if from == 0 {
		from = to - 24*3600
	}
	pv, _ := strconv.ParseUint(q.Get("port"), 10, 16)
	rows, err := m.history.query(tier, from, to, uint16(pv))
	if err != nil {
		bad(w, 400, err)
		return
	}
	events, err := m.history.events(from, to)
	if err != nil {
		bad(w, 500, err)
		return
	}
	if q.Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=brutal-canary-history.csv")
		csvw := csv.NewWriter(w)
		csvw.Write([]string{"time_unix", "port", "group", "sent_bytes", "acked_bytes", "retrans_bytes", "retrans_percent", "success", "failure", "members", "rtt_mean_us", "rtt_max_us", "gap", "event", "expected_bytes", "actual_bytes"})
		for _, x := range rows {
			retrans := ""
			if x.Sent > 0 {
				retrans = fmt.Sprintf("%.4f", 100*float64(x.Retrans)/float64(x.Sent))
			}
			mean := ""
			if x.RTTSamples > 0 {
				mean = fmt.Sprintf("%d", x.RTTSum/x.RTTSamples)
			}
			csvw.Write([]string{strconv.FormatInt(x.Time, 10), strconv.Itoa(int(x.Port)), strconv.FormatUint(x.Group, 10), strconv.FormatUint(x.Sent, 10), strconv.FormatUint(x.Acked, 10), strconv.FormatUint(x.Retrans, 10), retrans, strconv.FormatUint(x.Success, 10), strconv.FormatUint(x.Failure, 10), strconv.Itoa(int(x.Members)), mean, strconv.Itoa(int(x.RTTMax)), strconv.FormatBool(x.Gap), "", strconv.FormatUint(x.Expected, 10), strconv.FormatUint(x.Actual, 10)})
		}
		for _, x := range events {
			csvw.Write([]string{strconv.FormatInt(x.Time, 10), "", "", "", "", "", "", "", "", "", "", "", "", "", x.Kind + ":" + x.Detail, "", ""})
		}
		csvw.Flush()
		return
	}
	jsonReply(w, 200, map[string]any{"tier": tier, "samples": rows, "events": events})
}

func (m *manager) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, 400, err)
		return
	}
	m.mu.Lock()
	next := m.cfg
	if err := persistPassword(&next, in.Password); err != nil {
		m.mu.Unlock()
		bad(w, 400, err)
		return
	}
	m.cfg = next
	m.sessions = map[string]session{}
	m.attempts = map[string]attempt{}
	m.mu.Unlock()
	m.history.addEvent("password_change", map[string]bool{"changed": true})
	jsonReply(w, 200, map[string]bool{"changed": true})
}

func (m *manager) resetPassword(w http.ResponseWriter, r *http.Request) {
	password, err := randomToken(18)
	if err != nil {
		bad(w, 500, err)
		return
	}
	m.mu.Lock()
	next := m.cfg
	if err = persistPassword(&next, password); err != nil {
		m.mu.Unlock()
		bad(w, 500, err)
		return
	}
	m.cfg = next
	m.sessions = map[string]session{}
	m.attempts = map[string]attempt{}
	m.mu.Unlock()
	m.history.addEvent("password_reset", map[string]bool{"reset": true})
	jsonReply(w, 200, map[string]string{"password": password})
}

func (m *manager) settings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Host       string   `json:"host"`
		Port       uint16   `json:"port"`
		AllowedIPs []string `json:"allowed_ips"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, 400, err)
		return
	}
	if net.ParseIP(in.Host) == nil || in.Port < 1024 {
		bad(w, 400, errors.New("invalid listener"))
		return
	}
	for _, ip := range in.AllowedIPs {
		if net.ParseIP(ip) == nil {
			bad(w, 400, errors.New("invalid allowed IP"))
			return
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if in.Port != m.cfg.WebPort {
		listener, err := net.Listen("tcp", net.JoinHostPort(in.Host, strconv.Itoa(int(in.Port))))
		if err != nil {
			bad(w, 409, fmt.Errorf("panel port unavailable: %w", err))
			return
		}
		listener.Close()
	}
	for _, p := range m.cfg.Ports {
		if p.Enabled && p.Port == in.Port {
			bad(w, 400, errors.New("panel port is managed"))
			return
		}
	}
	next := m.cfg
	next.WebHost = in.Host
	next.WebPort = in.Port
	next.AllowedIPs = in.AllowedIPs
	group, err := user.LookupGroup("tcpbrutal")
	if err != nil {
		bad(w, 500, err)
		return
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		bad(w, 500, err)
		return
	}
	if err = saveConfig(next); err != nil {
		bad(w, 500, err)
		return
	}
	if err = writePublicWebConfig(next, gid); err != nil {
		saveConfig(m.cfg)
		bad(w, 500, err)
		return
	}
	m.cfg = next
	m.history.addEvent("panel_settings", map[string]any{"host": in.Host, "port": in.Port, "allowed_ips": in.AllowedIPs})
	jsonReply(w, 200, map[string]any{"saved": true, "restart_required": true})
}

func (m *manager) autostart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		State string `json:"state"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, 400, err)
		return
	}
	verb := "enable"
	if in.State == "off" {
		verb = "disable"
	} else if in.State != "on" {
		bad(w, 400, errors.New("invalid state"))
		return
	}
	out, err := exec.Command("systemctl", verb, "tcp-brutal-canary-manager.service", "tcp-brutal-canary-web.service").CombinedOutput()
	if err != nil {
		bad(w, 500, fmt.Errorf("%s: %w", out, err))
		return
	}
	m.history.addEvent("autostart", in)
	jsonReply(w, 200, map[string]string{"state": in.State})
}

func autostartState(query func(string) (string, error)) map[string]any {
	services := []string{"tcp-brutal-canary-manager.service", "tcp-brutal-canary-web.service"}
	states := make([]bool, len(services))
	unknown := false
	for i, service := range services {
		value, err := query(service)
		value = strings.TrimSpace(value)
		if err != nil && value != "disabled" && value != "static" && value != "masked" {
			unknown = true
		}
		states[i] = value == "enabled"
	}
	state := "off"
	if unknown {
		state = "unknown"
	} else if states[0] && states[1] {
		state = "on"
	} else if states[0] || states[1] {
		state = "partial"
	}
	return map[string]any{"state": state, "manager": states[0], "web": states[1]}
}

func (m *manager) autostartStatus(w http.ResponseWriter, r *http.Request) {
	jsonReply(w, 200, autostartState(func(service string) (string, error) {
		out, err := exec.Command("systemctl", "is-enabled", service).CombinedOutput()
		return string(out), err
	}))
}

func (m *manager) startUpdate(w http.ResponseWriter, r *http.Request) {
	bad(w, http.StatusNotImplemented, errors.New("Canary self-update is disabled; use install-canary.sh from the canary branch"))
}


func (m *manager) updateJobState(state, detail string) {
	m.mu.Lock()
	m.job.State = state
	m.job.Detail = detail
	m.mu.Unlock()
	m.history.addEvent("update", map[string]string{"state": state, "detail": detail})
}

type updateJob struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Detail  string `json:"detail"`
	Started int64  `json:"started"`
}

func (m *manager) checkUpdate(w http.ResponseWriter, r *http.Request) {
	jsonReply(w, http.StatusOK, map[string]string{
		"installed": version,
		"latest":    version,
		"notes":     "Canary releases are managed separately from baseline; use install-canary.sh.",
		"url":       "",
	})
}


func (m *manager) performUpdate(id string) {
	m.updateJobState("failed", "Canary self-update is disabled; use install-canary.sh")
}
