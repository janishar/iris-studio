package server

import (
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type Config struct {
	mu       sync.RWMutex
	Root     string
	Static   string
	Sessions string
	// Platform is helmstudio, the one main resolved from the environment it
	// was launched with. Never nil outside tests: main will not start the
	// studio without it.
	Platform    *Platform
	IrisFile    string
	ModelFile   string
	SettingFile string
	Iris        string
	Model       string
	Workdir     string
	// allowedHosts is the set of Host header names this server answers to,
	// besides IP literals. Written once by NewConfig and never after, so
	// guard reads it without the lock.
	allowedHosts  map[string]bool
	activeSession string
	inputs        string
	outputs       string
}

type Args struct {
	Iris  string
	Model string
	// Root is the directory iris studio keeps sessions in: helmstudio's data
	// directory for this studio, passed as {data}. It makes none of its own,
	// and adoption depends on this — helmstudio hardlinks a take from inside
	// the studio's own data directory and refuses a path outside it.
	Root string
	// Platform is helmstudio, resolved by main before anything else.
	Platform *Platform
	// Host is the address the server binds to, from --host. A name (rather
	// than an IP literal) is accepted as a Host header as well.
	Host string
	// AllowedHosts are extra Host header names accepted besides IP literals
	// and localhost, from --allow-host.
	AllowedHosts []string
}

// discoverStatic finds the page iris studio serves: static/ beside the
// binary, one level up from it, or in the working directory.
//
// It is deliberately not looked for under --root. That is helmstudio's data
// directory — what this studio writes — and holds no source; the page belongs
// to the build, wherever the build put it.
func discoverStatic() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	candidates := []string{filepath.Join(dir, "static"), filepath.Join(filepath.Dir(dir), "static")}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "static"))
	}
	for _, static := range candidates {
		if FileExists(filepath.Join(static, "index.html")) {
			return static, nil
		}
	}
	return candidates[0], nil
}

func NewConfig(args Args) (*Config, error) {
	static, err := discoverStatic()
	if err != nil {
		return nil, err
	}
	iris, err := filepath.Abs(expandHome(args.Iris))
	if err != nil {
		return nil, err
	}
	model, err := filepath.Abs(expandHome(args.Model))
	if err != nil {
		return nil, err
	}
	// Absolute, because a take's path is what helmstudio is asked to adopt and
	// it will not adopt a relative one.
	root, err := filepath.Abs(expandHome(args.Root))
	if err != nil {
		return nil, err
	}
	sessions := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		return nil, err
	}
	cfg := &Config{
		Root:        root,
		Static:      static,
		Platform:    args.Platform,
		Sessions:    sessions,
		IrisFile:    filepath.Join(sessions, "iris.json"),
		ModelFile:   filepath.Join(sessions, "model.json"),
		SettingFile: filepath.Join(sessions, "last_session.json"),
		Iris:        iris,
		Model:       model,
		Workdir:     filepath.Dir(iris),

		allowedHosts: map[string]bool{"localhost": true},
	}
	for _, host := range args.AllowedHosts {
		if host = strings.ToLower(stringsTrimSpace(host)); host != "" {
			cfg.allowedHosts[host] = true
		}
	}
	if host := strings.ToLower(stringsTrimSpace(args.Host)); host != "" && net.ParseIP(host) == nil {
		cfg.allowedHosts[host] = true
	}
	if saved, ok := readStringField(cfg.IrisFile, "iris"); ok {
		if abs, err := filepath.Abs(expandHome(saved)); err == nil {
			cfg.Iris = abs
			cfg.Workdir = filepath.Dir(abs)
		}
	}
	if saved, ok := readStringField(cfg.ModelFile, "model"); ok {
		if abs, err := filepath.Abs(expandHome(saved)); err == nil {
			cfg.Model = abs
		}
	}
	_ = WriteJSONFile(cfg.ModelFile, map[string]any{"model": cfg.Model}, true)
	_ = WriteJSONFile(cfg.IrisFile, map[string]any{"iris": cfg.Iris}, true)
	cfg.activeSession = cfg.loadLastSession()
	if _, _, err := cfg.ActivateSession(cfg.activeSession); err != nil {
		return nil, err
	}
	setting := cfg.SessionSetting(cfg.activeSession)
	current := readJSONObject(setting)
	defaults := defaultSessionSettings(cfg.activeSession)
	for k, v := range current {
		defaults[k] = v
	}
	if _, ok := defaults["takes"].([]any); !ok {
		defaults["takes"] = []any{}
	}
	if err := WriteJSONFile(setting, defaults, true); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) loadLastSession() string {
	if data := readJSONObject(c.SettingFile); len(data) > 0 {
		raw := stringsTrimSpace(anyToString(data["last_session"]))
		if raw != "" {
			name := safeStem(raw)
			if DirExists(filepath.Join(c.Sessions, name)) {
				return name
			}
		}
	}
	entries, err := os.ReadDir(c.Sessions)
	if err == nil {
		var existing []string
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			root := filepath.Join(c.Sessions, entry.Name())
			if DirExists(filepath.Join(root, "inputs")) && DirExists(filepath.Join(root, "outputs")) {
				existing = append(existing, entry.Name())
			}
		}
		sort.Strings(existing)
		if len(existing) > 0 {
			return existing[0]
		}
	}
	return "session-1"
}

func (c *Config) ActivateSession(name string) (string, string, error) {
	if stringsTrimSpace(name) == "" {
		name = "session-1"
	}
	session := safeStem(name)
	root := filepath.Join(c.Sessions, session)
	inputs := filepath.Join(root, "inputs")
	outputs := filepath.Join(root, "outputs")
	if err := os.MkdirAll(inputs, 0o755); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(outputs, 0o755); err != nil {
		return "", "", err
	}
	c.mu.Lock()
	changed := c.activeSession != session
	c.activeSession = session
	c.inputs = inputs
	c.outputs = outputs
	c.mu.Unlock()
	if err := WriteJSONFile(c.SettingFile, map[string]any{"last_session": session}, true); err != nil {
		return "", "", err
	}
	setting := c.SessionSetting(session)
	if !FileExists(setting) {
		if err := WriteJSONFile(setting, defaultSessionSettings(session), true); err != nil {
			return "", "", err
		}
	}
	if changed {
		terminalLogPath := c.TerminalLog("")
		if FileExists(terminalLogPath) {
			if err := os.WriteFile(terminalLogPath, []byte{}, 0o644); err != nil {
				return "", "", err
			}
		}
	}
	if err := ensureFile(c.TerminalLog(session)); err != nil {
		return "", "", err
	}
	return inputs, outputs, nil
}

func (c *Config) SessionSetting(name string) string {
	if stringsTrimSpace(name) == "" {
		name = "session-1"
	}
	session := safeStem(name)
	root := filepath.Join(c.Sessions, session)
	_ = os.MkdirAll(root, 0o755)
	return filepath.Join(root, "setting.json")
}

func (c *Config) SessionDirs(name string) (string, string, error) {
	if stringsTrimSpace(name) == "" {
		name = "default"
	}
	session := safeStem(name)
	root := filepath.Join(c.Sessions, session)
	inputs := filepath.Join(root, "inputs")
	outputs := filepath.Join(root, "outputs")
	if err := os.MkdirAll(inputs, 0o755); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(outputs, 0o755); err != nil {
		return "", "", err
	}
	return inputs, outputs, nil
}

func (c *Config) TerminalLog(name string) string {
	c.mu.RLock()
	active := c.activeSession
	c.mu.RUnlock()
	if stringsTrimSpace(name) == "" {
		name = active
	}
	if stringsTrimSpace(name) == "" {
		name = "session-1"
	}
	session := safeStem(name)
	path := filepath.Join(c.Sessions, session, "terminal.log")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	return path
}

func (c *Config) Snapshot() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return map[string]string{
		"iris":    c.Iris,
		"model":   c.Model,
		"workdir": c.Workdir,
		"inputs":  c.inputs,
		"outputs": c.outputs,
		"session": c.activeSession,
	}
}

func (c *Config) CurrentSession() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.activeSession
}

func (c *Config) CurrentInputs() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.inputs
}

func (c *Config) CurrentOutputs() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.outputs
}

// SavedPath is a path iris studio remembered under --root the last time it
// ran, or "" when it remembered none. main reads it so a second run can omit
// the flags a first run needed.
func SavedPath(root, file, field string) string {
	value, _ := readStringField(filepath.Join(root, "sessions", file), field)
	return value
}

// hostAllowed reports whether a request's Host header names this server: an IP
// literal, localhost, the --host name or an --allow-host name. Other names are
// refused, so a DNS name that resolves to 127.0.0.1 cannot be used to reach
// this API from a page the browser thinks is somewhere else.
func (c *Config) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	return c.allowedHosts[host]
}
