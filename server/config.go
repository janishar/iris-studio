package server

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type Config struct {
	mu            sync.RWMutex
	Root          string
	Static        string
	Sessions      string
	IrisFile      string
	ModelFile     string
	SettingFile   string
	Iris          string
	Model         string
	Workdir       string
	activeSession string
	inputs        string
	outputs       string
}

type Args struct {
	Iris  string
	Model string
}

func discoverRoot() (string, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	root := filepath.Dir(exe)
	static := filepath.Join(root, "static")
	if FileExists(filepath.Join(static, "index.html")) {
		return root, static, nil
	}
	parent := filepath.Dir(root)
	parentStatic := filepath.Join(parent, "static")
	if parent != root && FileExists(filepath.Join(parentStatic, "index.html")) {
		return parent, parentStatic, nil
	}
	cwd, err := os.Getwd()
	if err == nil {
		cwdStatic := filepath.Join(cwd, "static")
		if FileExists(filepath.Join(cwdStatic, "index.html")) {
			return cwd, cwdStatic, nil
		}
	}
	return root, static, nil
}

func NewConfig(args Args) (*Config, error) {
	root, static, err := discoverRoot()
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
	sessions := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		return nil, err
	}
	cfg := &Config{
		Root:        root,
		Static:      static,
		Sessions:    sessions,
		IrisFile:    filepath.Join(sessions, "iris.json"),
		ModelFile:   filepath.Join(sessions, "model.json"),
		SettingFile: filepath.Join(sessions, "last_session.json"),
		Iris:        iris,
		Model:       model,
		Workdir:     filepath.Dir(iris),
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
