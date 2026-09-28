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
	if err = os.MkdirAll("/run/tcp-brutal-custom", 0750); err != nil {
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
	os.Chown("/run/tcp-brutal-custom", 0, gid)
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

func parsePorts() ([]portState, error) {
	b, err := os.ReadFile(portsPath)
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

func (m *manager) applyPort(p portConfig) error {
	if err := validatePort(p, m.cfg.WebPort); err != nil {
		return err
	}
	rate := uint64(p.RateMbps*1e6/8 + 0.5)
	if err := writePort(fmt.Sprintf("add %d rate=%d gain=%d", p.Port, rate, p.Gain)); err != nil {
		return err
	}
	if err := m.selector.Enable(p.Port); err != nil {
		return err
	}
	return nil
}

func (m *manager) disablePort(port uint16) error {
	if err := m.selector.Disable(port); err != nil {
		return err
	}
	if err := writePort(fmt.Sprintf("del %d", port)); err != nil {
		m.selector.Enable(port)
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
			states, err := parsePorts()
			if err != nil {
				log.Printf("sampling: %v", err)
				continue
			}
			now := time.Now().Unix()
			for _, p := range states {
				var c selectorCount
				if p.Active {
					c, err = m.selector.Count(p.Port)
					if err != nil {
						log.Printf("selector count: %v", err)
						continue
					}
				}
				x := sample{Time: now, Port: p.Port, Group: p.Group, Sent: p.Sent, Acked: p.Acked, Retrans: p.Retrans, Success: c.Success, Failure: c.Failure, Members: p.Members, RTTSum: p.RTTSum, RTTSamples: p.RTTSamples, RTTMax: p.RTTMax}
				if err = m.history.record(x); err != nil {
					log.Printf("history: %v", err)
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
		w.Header().Set("Content-Disposition", "attachment; filename=brutal-history.csv")
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
	out, err := exec.Command("systemctl", verb, "tcp-brutal-custom-manager.service", "tcp-brutal-custom-web.service").CombinedOutput()
	if err != nil {
		bad(w, 500, fmt.Errorf("%s: %w", out, err))
		return
	}
	m.history.addEvent("autostart", in)
	jsonReply(w, 200, map[string]string{"state": in.State})
}

func autostartState(query func(string) (string, error)) map[string]any {
	services := []string{"tcp-brutal-custom-manager.service", "tcp-brutal-custom-web.service"}
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
	// Maintenance update disconnects managed TCP sessions and replaces a kernel
	// module. Keep that high-impact action local to a root caller on the Unix
	// socket; authenticated Web sessions may only check for updates.
	if r.Context().Value(peerKey{}) != uint32(0) {
		bad(w, 403, errors.New("maintenance update requires local root CLI"))
		return
	}
	m.mu.Lock()
	if m.job.State == "running" || exec.Command("systemctl", "is-active", "--quiet", "tcp-brutal-custom-update.service").Run() == nil {
		m.mu.Unlock()
		bad(w, 409, errors.New("update already running"))
		return
	}
	id, err := randomToken(8)
	if err != nil {
		m.mu.Unlock()
		bad(w, 500, err)
		return
	}
	if err = os.WriteFile(dataDir+"/update-id", []byte(id), 0600); err != nil {
		m.mu.Unlock()
		bad(w, 500, err)
		return
	}
	m.job = updateJob{ID: id, State: "running", Started: time.Now().Unix()}
	m.mu.Unlock()
	go m.performUpdate(id)
	jsonReply(w, 202, map[string]string{"job_id": id})
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
	client := &http.Client{Timeout: 8 * time.Second}
	request, err := http.NewRequestWithContext(r.Context(), "GET", "https://api.github.com/repos/sagehere/tcp-brutal-custom/releases/latest", nil)
	if err != nil {
		bad(w, 500, err)
		return
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := client.Do(request)
	if err != nil {
		bad(w, 502, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		bad(w, 502, fmt.Errorf("GitHub returned %d", response.StatusCode))
		return
	}
	var release struct {
		Tag  string `json:"tag_name"`
		Body string `json:"body"`
		URL  string `json:"html_url"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&release); err != nil {
		bad(w, 502, err)
		return
	}
	jsonReply(w, 200, map[string]string{"installed": version, "latest": release.Tag, "notes": release.Body, "url": release.URL})
}

func (m *manager) performUpdate(id string) {
	// A dedicated systemd unit owns module replacement so this manager can
	// report the job and the work survives a browser disconnect.
	out, err := exec.Command("systemctl", "start", "--no-block", "tcp-brutal-custom-update.service").CombinedOutput()
	if err != nil {
		m.updateJobState("failed", fmt.Sprintf("%s: %v", out, err))
		return
	}
	m.updateJobState("running", "maintenance unit started")
}
