package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	configDir         = "/etc/tcp-brutal-custom"
	dataDir           = "/var/lib/tcp-brutal-custom"
	socketPath        = "/run/tcp-brutal-custom/manager.sock"
	portsPath         = "/proc/net/tcp_brutal/ports"
	panelPasswordFile = "/etc/tcp-brutal-custom/panel-password"
)

var version = "2.1.9-dev"

type portConfig struct {
	Port     uint16  `json:"port"`
	RateMbps float64 `json:"rate_mbps"`
	Gain     uint32  `json:"gain"`
	Enabled  bool    `json:"enabled"`
}

type config struct {
	WebHost      string       `json:"web_host"`
	WebPort      uint16       `json:"web_port"`
	AllowedIPs   []string     `json:"allowed_ips"`
	PasswordSalt string       `json:"password_salt"`
	PasswordHash string       `json:"password_hash"`
	Ports        []portConfig `json:"ports"`
}

func configPath() string { return filepath.Join(configDir, "config.json") }

func loadConfig() (config, error) {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return config{}, err
	}
	var c config
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.WebHost == "" || c.WebPort == 0 {
		return c, errors.New("invalid panel address")
	}
	return c, nil
}

func saveConfig(c config) error {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(configDir, ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), configPath())
}

func savePanelPassword(password string) error {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(configDir, ".panel-password-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.WriteString(password + "\n"); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, panelPasswordFile)
}

func loadPanelPassword() (string, error) {
	b, err := os.ReadFile(panelPasswordFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("当前密码不可恢复；这是旧版本安装，请先执行“重置登录密码”")
		}
		return "", err
	}
	password := strings.TrimSpace(string(b))
	if password == "" {
		return "", errors.New("当前密码记录为空，请重置登录密码")
	}
	return password, nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func setPassword(c *config, password string) error {
	if utf8.RuneCountInString(password) < 8 {
		return errors.New("password needs at least 8 characters")
	}
	salt, err := randomToken(16)
	if err != nil {
		return err
	}
	c.PasswordSalt = salt
	saltBytes, _ := hex.DecodeString(salt)
	c.PasswordHash = hex.EncodeToString(argon2.IDKey([]byte(password), saltBytes, 3, 64*1024, 4, 32))
	return nil
}

func passwordMatches(c config, password string) bool {
	salt, err := hex.DecodeString(c.PasswordSalt)
	if err != nil {
		return false
	}
	hash, err := hex.DecodeString(c.PasswordHash)
	if err != nil || len(hash) == 0 {
		return false
	}
	candidate := argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32)
	return subtle.ConstantTimeCompare(candidate, hash) == 1
}

func persistPassword(c *config, password string) error {
	previous := *c
	next := *c
	if err := setPassword(&next, password); err != nil {
		return err
	}
	if err := saveConfig(next); err != nil {
		return err
	}
	if err := savePanelPassword(password); err != nil {
		_ = saveConfig(previous)
		return err
	}
	*c = next
	return nil
}

func initConfig(webPort uint16) error {
	if os.Geteuid() != 0 {
		return errors.New("root required")
	}
	if webPort < 1024 {
		return errors.New("invalid panel port")
	}
	if _, err := os.Stat(configPath()); err == nil {
		return errors.New("configuration already exists")
	}
	password, err := randomToken(18)
	if err != nil {
		return err
	}
	c := config{WebHost: "0.0.0.0", WebPort: webPort}
	if err = setPassword(&c, password); err != nil {
		return err
	}
	if err = saveConfig(c); err != nil {
		return err
	}
	if err = savePanelPassword(password); err != nil {
		os.Remove(configPath())
		return err
	}
	fmt.Println("Panel admin password:", password)
	return nil
}

func webMode() error {
	c, err := loadPublicWebConfig()
	if err != nil {
		return err
	}
	target, _ := url.Parse("http://unix")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", socketPath)
	}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "bad peer", 400)
			return
		}
		if len(c.AllowedIPs) != 0 {
			allowed := false
			for _, candidate := range c.AllowedIPs {
				if net.ParseIP(candidate).Equal(net.ParseIP(ip)) {
					allowed = true
					break
				}
			}
			if !allowed {
				http.Error(w, "forbidden", 403)
				return
			}
		}
		r.Header.Del("X-Forwarded-For")
		r.Header.Del("X-Client-IP")
		r.Header.Set("X-Client-IP", ip)
		proxy.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: net.JoinHostPort(c.WebHost, strconv.Itoa(int(c.WebPort))), Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	return server.ListenAndServe()
}

type publicWebConfig struct {
	WebHost    string   `json:"web_host"`
	WebPort    uint16   `json:"web_port"`
	AllowedIPs []string `json:"allowed_ips"`
}

func loadPublicWebConfig() (publicWebConfig, error) {
	b, err := os.ReadFile("/run/tcp-brutal-custom/panel.json")
	if err != nil {
		return publicWebConfig{}, err
	}
	var c publicWebConfig
	err = json.Unmarshal(b, &c)
	return c, err
}

func writePublicWebConfig(c config, gid int) error {
	p := "/run/tcp-brutal-custom/panel.json"
	b, err := json.Marshal(publicWebConfig{c.WebHost, c.WebPort, c.AllowedIPs})
	if err != nil {
		return err
	}
	if err = os.WriteFile(p, b, 0640); err != nil {
		return err
	}
	if err = os.Chown(p, 0, gid); err != nil {
		return err
	}
	return os.Chmod(p, 0640)
}

func client() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) { return net.Dial("unix", socketPath) },
	}}
}

func localRequest(method, path string, body io.Reader) error {
	req, err := http.NewRequest(method, "http://unix"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(os.Stdout, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func resetPanelPasswordCLI() error {
	resp, err := client().Post("http://unix/api/v1/password/reset", "application/json", strings.NewReader("{}"))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Password string `json:"password"`
		Error    string `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&out); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		if out.Error != "" {
			return errors.New(out.Error)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out.Password == "" {
		return errors.New("服务器未返回新密码")
	}
	fmt.Println("登录密码已重置。")
	fmt.Println("新的面板登录密码:", out.Password)
	fmt.Println("请立即保存该密码。")
	return nil
}

func runCLI(args []string) error {
	if os.Geteuid() != 0 {
		return errors.New("root required")
	}
	if len(args) == 0 {
		return menu()
	}
	switch args[0] {
	case "backup":
		path, err := backup()
		if err == nil {
			fmt.Println(path)
		}
		return err
	case "restore":
		if len(args) == 2 {
			return restore(args[1])
		}
	case "install":
		cmd := exec.Command("/usr/local/lib/tcp-brutal-custom/install.sh", "install")
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	case "uninstall":
		out, err := exec.Command("systemctl", "start", "--no-block", "tcp-brutal-custom-uninstall.service").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
		}
		fmt.Println("Graceful uninstall started. Existing Brutal connections will drain naturally; removal completes automatically.")
		return nil
	case "status", "diagnose":
		return localRequest("GET", "/api/v1/status", nil)
	case "ports":
		return localRequest("GET", "/api/v1/ports", nil)
	case "port":
		if len(args) >= 3 && args[1] == "del" {
			return localRequest("DELETE", "/api/v1/ports/"+args[2], nil)
		}
		if len(args) >= 4 && args[1] == "add" {
			port, e := strconv.ParseUint(args[2], 10, 16)
			if e != nil {
				return e
			}
			rate, e := strconv.ParseFloat(args[3], 64)
			if e != nil {
				return e
			}
			gain := uint32(20)
			if len(args) > 4 {
				v, e := strconv.ParseUint(strings.TrimPrefix(args[4], "gain="), 10, 32)
				if e != nil {
					return e
				}
				gain = uint32(v)
			}
			b, _ := json.Marshal(portConfig{Port: uint16(port), RateMbps: rate, Gain: gain, Enabled: true})
			return localRequest("POST", "/api/v1/ports", strings.NewReader(string(b)))
		}
	case "password":
		if len(args) == 2 && args[1] == "show" {
			password, err := loadPanelPassword()
			if err != nil {
				return err
			}
			fmt.Println("当前面板登录密码:", password)
			return nil
		}
		if len(args) == 2 && args[1] == "reset" {
			return resetPanelPasswordCLI()
		}
		if len(args) == 3 && args[1] == "set" {
			b, _ := json.Marshal(map[string]string{"password": args[2]})
			return localRequest("PUT", "/api/v1/password", strings.NewReader(string(b)))
		}
		// Keep backward compatibility with: tbc2 password NEW
		if len(args) == 2 {
			b, _ := json.Marshal(map[string]string{"password": args[1]})
			return localRequest("PUT", "/api/v1/password", strings.NewReader(string(b)))
		}
	case "update":
		return localRequest("POST", "/api/v1/update", strings.NewReader("{}"))
	case "panel":
		if len(args) == 4 {
			port, e := strconv.ParseUint(args[2], 10, 16)
			if e != nil {
				return e
			}
			var ips []string
			if args[3] != "-" {
				for _, ip := range strings.Split(args[3], ",") {
					ips = append(ips, strings.TrimSpace(ip))
				}
			}
			b, _ := json.Marshal(map[string]any{"host": args[1], "port": port, "allowed_ips": ips})
			return localRequest("PUT", "/api/v1/settings", strings.NewReader(string(b)))
		}
	case "autostart":
		if len(args) == 2 && (args[1] == "on" || args[1] == "off") {
			b, _ := json.Marshal(map[string]string{"state": args[1]})
			return localRequest("PUT", "/api/v1/autostart", strings.NewReader(string(b)))
		}
	}
	return errors.New("usage: tbc2 [manager|web|init [panel-port]|status|ports|port add PORT Mbps [gain=20]|port del PORT|password NEW|update|autostart on|autostart off]")
}

func panelManagementMenu() error {
	for {
		fmt.Println("\n===== 面板管理 =====")
		fmt.Println("1  面板网络设置")
		fmt.Println("2  查看当前登录密码")
		fmt.Println("3  重置登录密码")
		fmt.Println("4  修改登录密码")
		fmt.Println("0  返回主菜单")
		fmt.Print("\n请选择操作: ")
		var choice string
		fmt.Scanln(&choice)
		var err error
		switch choice {
		case "0":
			return nil
		case "1":
			var host, port, allow string
			fmt.Print("监听 IP: ")
			fmt.Scanln(&host)
			fmt.Print("面板端口: ")
			fmt.Scanln(&port)
			fmt.Print("允许访问的 IP（逗号分隔，输入 - 表示不限制）: ")
			fmt.Scanln(&allow)
			err = runCLI([]string{"panel", host, port, allow})
		case "2":
			err = runCLI([]string{"password", "show"})
		case "3":
			fmt.Print("确认重置登录密码？输入 YES 继续: ")
			var confirm string
			fmt.Scanln(&confirm)
			if confirm != "YES" {
				fmt.Println("已取消。")
				continue
			}
			err = runCLI([]string{"password", "reset"})
		case "4":
			fmt.Print("请输入新密码（至少 8 个字符）: ")
			var password string
			fmt.Scanln(&password)
			err = runCLI([]string{"password", "set", password})
			if err == nil {
				fmt.Println("登录密码已修改。")
			}
		default:
			err = errors.New("无效选项")
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "操作失败:", err)
		}
	}
}

func menu() error {
	for {
		fmt.Printf("TCP Brutal Custom    v%s\n\n1  安装\n2  查看状态\n3  查看接管端口\n4  新增接管端口\n5  删除接管端口\n6  检查并升级\n7  开启开机启动\n8  关闭开机启动\n9  面板管理\n10 卸载\n11 备份配置\n12 恢复配置\n0  退出\n\n", version)
		var choice, port, rate string
		fmt.Print("请选择操作: ")
		fmt.Scanln(&choice)
		var err error
		switch choice {
		case "0":
			return nil
		case "1":
			err = runCLI([]string{"install"})
		case "2":
			err = runCLI([]string{"status"})
		case "3":
			err = runCLI([]string{"ports"})
		case "4":
			fmt.Print("请输入端口: ")
			fmt.Scanln(&port)
			fmt.Print("请输入目标速率 Mbps: ")
			fmt.Scanln(&rate)
			err = runCLI([]string{"port", "add", port, rate})
		case "5":
			fmt.Print("请输入要删除的端口: ")
			fmt.Scanln(&port)
			err = runCLI([]string{"port", "del", port})
		case "6":
			err = runCLI([]string{"update"})
		case "7":
			err = runCLI([]string{"autostart", "on"})
		case "8":
			err = runCLI([]string{"autostart", "off"})
		case "9":
			err = panelManagementMenu()
		case "10":
			err = runCLI([]string{"uninstall"})
		case "11":
			err = runCLI([]string{"backup"})
		case "12":
			fmt.Print("请输入备份文件名: ")
			fmt.Scanln(&port)
			err = runCLI([]string{"restore", port})
		default:
			err = errors.New("无效选项")
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "操作失败:", err)
		}
	}
}

func main() {
	var err error
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "manager":
			err = managerMode()
		case "web":
			err = webMode()
		case "probe-bpf":
			err = probeSelector()
		case "init":
			port := uint64(23333)
			if len(os.Args) > 2 {
				port, err = strconv.ParseUint(os.Args[2], 10, 16)
			}
			if err == nil {
				err = initConfig(uint16(port))
			}
		default:
			err = runCLI(os.Args[1:])
		}
	} else {
		err = runCLI(nil)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func waitSignal(cancel context.CancelFunc) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	cancel()
}
