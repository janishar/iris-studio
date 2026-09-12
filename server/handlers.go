package server

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type App struct {
	cfg    *Config
	runner *Runner
}

func NewApp(cfg *Config, runner *Runner) *App {
	return &App{cfg: cfg, runner: runner}
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	unescaped, _ := url.PathUnescape(path)
	switch r.Method {
	case http.MethodGet:
		a.handleGet(w, r, unescaped)
	case http.MethodPost:
		a.handlePost(w, r, unescaped)
	default:
		a.send(w, http.StatusNotFound, []byte(`{"error":"not found"}`), "application/json", nil)
	}
}

func (a *App) handleGet(w http.ResponseWriter, r *http.Request, p string) {
	if p == "/" {
		a.serveFile(w, r, filepath.Join(a.cfg.Static, "index.html"), false)
		return
	}
	if strings.HasPrefix(p, "/static/") {
		target := filepath.Clean(filepath.Join(a.cfg.Static, strings.TrimPrefix(p, "/static/")))
		if !strings.HasPrefix(target, a.cfg.Static+string(os.PathSeparator)) && target != a.cfg.Static {
			a.send(w, http.StatusForbidden, []byte(`{"error":"forbidden"}`), "application/json", nil)
			return
		}
		a.serveFile(w, r, target, false)
		return
	}
	if p == "/api/config" {
		snap := a.cfg.Snapshot()
		modelLabel, modelFamily := labelFor(filepath.Base(snap["model"]))
		a.json(w, map[string]any{
			"iris":            snap["iris"],
			"model":           snap["model"],
			"model_label":     modelLabel,
			"model_family":    modelFamily,
			"model_valid":     isModelDir(snap["model"]),
			"workdir":         snap["workdir"],
			"inputs":          snap["inputs"],
			"outputs":         snap["outputs"],
			"session":         snap["session"],
			"setting":         a.cfg.SettingFile,
			"session_setting": a.cfg.SessionSetting(a.cfg.CurrentSession()),
			"min_dimension":   minDimension,
			"max_dimension":   maxDimension,
			"dim_step":        dimStep,
			"max_refs":        maxRefs,
		})
		return
	}
	if p == "/api/inputs" {
		a.json(w, listInputs(a.cfg))
		return
	}
	if p == "/api/outputs" {
		a.json(w, listOutputs(a.cfg))
		return
	}
	if p == "/api/sessions" {
		a.json(w, listSessions(a.cfg))
		return
	}
	if strings.HasPrefix(p, "/api/session/") {
		name := filepath.Base(strings.TrimPrefix(p, "/api/session/"))
		path := a.cfg.SessionSetting(name)
		if !FileExists(path) {
			a.send(w, http.StatusNotFound, []byte(`{"error":"session not found"}`), "application/json", nil)
			return
		}
		if _, _, err := a.cfg.ActivateSession(name); err != nil {
			a.jsonCode(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		data := readJSONObject(path)
		data["session_name"] = safeStem(name)
		data["input_path"] = a.cfg.CurrentInputs()
		data["output_path"] = a.cfg.CurrentOutputs()
		a.json(w, data)
		return
	}
	if p == "/api/queue" {
		a.json(w, map[string]any{"queue": a.runner.QueueState(), "history": a.runner.History(60)})
		return
	}
	if strings.HasPrefix(p, "/media/input/") {
		a.serveFile(w, r, filepath.Join(a.cfg.CurrentInputs(), filepath.Base(strings.TrimPrefix(p, "/media/input/"))), false)
		return
	}
	if strings.HasPrefix(p, "/media/output/") {
		a.serveFile(w, r, filepath.Join(a.cfg.CurrentOutputs(), filepath.Base(strings.TrimPrefix(p, "/media/output/"))), false)
		return
	}
	if strings.HasPrefix(p, "/download/") {
		a.serveFile(w, r, filepath.Join(a.cfg.CurrentOutputs(), filepath.Base(strings.TrimPrefix(p, "/download/"))), true)
		return
	}
	if p == "/api/events" {
		a.events(w, r)
		return
	}
	a.send(w, http.StatusNotFound, []byte(`{"error":"not found"}`), "application/json", nil)
}

func (a *App) handlePost(w http.ResponseWriter, r *http.Request, p string) {
	if p == "/api/upload" {
		a.upload(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	defer r.Body.Close()
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	data := map[string]any{}
	if len(body) > 0 {
		if err := decoder.Decode(&data); err != nil {
			a.jsonCode(w, map[string]any{"error": "bad json"}, http.StatusBadRequest)
			return
		}
	}
	switch p {
	case "/api/terminal":
		command := stringsTrimSpace(anyToString(data["command"]))
		if command == "" {
			a.jsonCode(w, map[string]any{"error": "command is required"}, http.StatusBadRequest)
			return
		}
		if !a.runner.RunTerminal(command) {
			a.jsonCode(w, map[string]any{"error": "terminal is already running a command"}, http.StatusConflict)
			return
		}
		a.json(w, map[string]any{"started": true})
	case "/api/interactive/load":
		ok, errMsg := a.runner.LoadInteractive(data)
		if errMsg != "" {
			a.jsonCode(w, map[string]any{"started": ok, "error": errMsg}, http.StatusConflict)
			return
		}
		a.json(w, map[string]any{"started": true})
	case "/api/interactive/input":
		ok, errMsg := a.runner.SendInteractive(anyToString(data["line"]))
		if errMsg != "" {
			a.jsonCode(w, map[string]any{"sent": ok, "error": errMsg}, http.StatusConflict)
			return
		}
		a.json(w, map[string]any{"sent": true})
	case "/api/interactive/stop":
		a.runner.StopInteractive()
		a.json(w, map[string]any{"stopped": true})
	case "/api/iris":
		iris, err := filepath.Abs(expandHome(anyToString(data["iris"])))
		info, statErr := os.Stat(iris)
		if err != nil || statErr != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			a.jsonCode(w, map[string]any{"error": "iris executable does not exist or is not executable"}, http.StatusBadRequest)
			return
		}
		if a.busyIris() {
			a.jsonCode(w, map[string]any{"error": "stop the active iris process before changing the binary"}, http.StatusConflict)
			return
		}
		a.cfg.Iris = iris
		a.cfg.Workdir = filepath.Dir(iris)
		_ = WriteJSONFile(a.cfg.IrisFile, map[string]any{"iris": iris}, true)
		a.json(w, map[string]any{"iris": iris})
	case "/api/model":
		model, err := filepath.Abs(expandHome(anyToString(data["model"])))
		if err != nil || !isModelDir(model) {
			a.jsonCode(w, map[string]any{"error": "not a valid iris model directory (expected transformer/ and vae/ subdirectories)"}, http.StatusBadRequest)
			return
		}
		if a.busyIris() {
			a.jsonCode(w, map[string]any{"error": "stop the active iris process before changing the model"}, http.StatusConflict)
			return
		}
		a.cfg.Model = model
		_ = WriteJSONFile(a.cfg.ModelFile, map[string]any{"model": model}, true)
		label, family := labelFor(filepath.Base(model))
		a.json(w, map[string]any{"model": model, "model_label": label, "model_family": family})
	case "/api/session/activate":
		name := safeStem(firstString(anyToString(data["name"]), "default"))
		inputs, outputs, err := a.cfg.ActivateSession(name)
		if err != nil {
			a.jsonCode(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		a.json(w, map[string]any{"name": name, "inputs": inputs, "outputs": outputs})
	case "/api/session/save":
		name, err := saveSession(a.cfg, data)
		if err != nil {
			a.jsonCode(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		inputs, outputs, err := a.cfg.ActivateSession(name)
		if err != nil {
			a.jsonCode(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		a.json(w, map[string]any{"name": name, "inputs": inputs, "outputs": outputs})
	case "/api/session/delete":
		name := safeStem(firstString(anyToString(data["name"]), a.cfg.CurrentSession()))
		next, err := deleteSession(a.cfg, name)
		if err != nil {
			a.jsonCode(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		inputs, outputs, err := a.cfg.ActivateSession(next)
		if err != nil {
			a.jsonCode(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		a.json(w, map[string]any{"name": next, "inputs": inputs, "outputs": outputs})
	case "/api/session/duplicate":
		src := safeStem(firstString(anyToString(data["name"]), a.cfg.CurrentSession()))
		dup, err := duplicateSession(a.cfg, src, anyToString(data["as"]))
		if err != nil {
			a.jsonCode(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		inputs, outputs, err := a.cfg.ActivateSession(dup)
		if err != nil {
			a.jsonCode(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		a.json(w, map[string]any{"name": dup, "inputs": inputs, "outputs": outputs})
	case "/api/render":
		if errs := validate(a.cfg, data); len(errs) > 0 {
			a.jsonCode(w, map[string]any{"errors": errs}, http.StatusBadRequest)
			return
		}
		name, err := saveSession(a.cfg, data)
		if err != nil {
			a.jsonCode(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		data["session_name"] = name
		if _, _, err := a.cfg.ActivateSession(name); err != nil {
			a.jsonCode(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		job := a.runner.Submit(data)
		a.json(w, job.Summary())
	case "/api/cancel":
		a.json(w, map[string]any{"ok": a.runner.Cancel(anyToString(data["id"]))})
	case "/api/use-ref":
		name, err := importOutputAsInput(a.cfg, anyToString(data["name"]))
		if err != nil {
			a.jsonCode(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		inputs := listInputs(a.cfg)
		a.runner.Emit("inputs", inputs)
		a.json(w, map[string]any{"name": name, "inputs": inputs})
	case "/api/delete":
		name := filepath.Base(anyToString(data["name"]))
		kind := anyToString(data["kind"])
		if name == "" || kind == "" {
			a.jsonCode(w, map[string]any{"error": "name and kind are required"}, http.StatusBadRequest)
			return
		}
		var filePath string
		switch kind {
		case "image":
			filePath = filepath.Join(a.cfg.CurrentInputs(), name)
		case "output":
			filePath = filepath.Join(a.cfg.CurrentOutputs(), name)
		default:
			a.jsonCode(w, map[string]any{"error": "invalid kind, must be 'image' or 'output'"}, http.StatusBadRequest)
			return
		}
		if !FileExists(filePath) {
			a.jsonCode(w, map[string]any{"error": "file not found"}, http.StatusNotFound)
			return
		}
		if err := os.Remove(filePath); err != nil {
			a.jsonCode(w, map[string]any{"error": err.Error()}, http.StatusInternalServerError)
			return
		}
		side := strings.TrimSuffix(filePath, filepath.Ext(filePath)) + ".json"
		if FileExists(side) {
			_ = os.Remove(side)
		}
		if kind == "output" {
			pruneTake(a.cfg, name)
		}
		inputs := listInputs(a.cfg)
		outputs := listOutputs(a.cfg)
		switch kind {
		case "image":
			a.runner.Emit("inputs", inputs)
		case "output":
			a.runner.Emit("outputs", outputs)
		}
		a.json(w, map[string]any{"inputs": inputs, "outputs": outputs})
	default:
		a.send(w, http.StatusNotFound, []byte(`{"error":"not found"}`), "application/json", nil)
	}
}

func (a *App) busyIris() bool {
	a.runner.mu.Lock()
	busy := a.runner.current != nil
	a.runner.mu.Unlock()
	a.runner.interactiveLock.Lock()
	defer a.runner.interactiveLock.Unlock()
	a.runner.normalizeInteractiveStateLocked()
	return busy || a.runner.interactiveProc != nil
}

func (a *App) upload(w http.ResponseWriter, r *http.Request) {
	name, _ := url.PathUnescape(r.Header.Get("X-Filename"))
	name = filepath.Base(firstString(name, "upload.png"))
	data, _ := io.ReadAll(r.Body)
	defer r.Body.Close()
	if len(data) == 0 {
		a.jsonCode(w, map[string]any{"error": "empty upload"}, http.StatusBadRequest)
		return
	}
	dst := filepath.Join(a.cfg.CurrentInputs(), name)
	base := strings.TrimSuffix(name, filepath.Ext(name))
	ext := filepath.Ext(name)
	for i := 1; FileExists(dst); i++ {
		dst = filepath.Join(a.cfg.CurrentInputs(), fmt.Sprintf("%s-%d%s", base, i, ext))
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		a.jsonCode(w, map[string]any{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	inputs := listInputs(a.cfg)
	a.runner.Emit("inputs", inputs)
	a.json(w, map[string]any{"name": filepath.Base(dst), "inputs": inputs})
}

func (a *App) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	q := a.runner.Subscribe()
	defer a.runner.Unsubscribe(q)
	headers := w.Header()
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	logData, _ := os.ReadFile(a.cfg.TerminalLog(a.cfg.CurrentSession()))
	hello, _ := json.Marshal(map[string]any{
		"kind": "hello",
		"payload": map[string]any{
			"queue":        a.runner.QueueState(),
			"history":      a.runner.History(60),
			"outputs":      listOutputs(a.cfg),
			"inputs":       listInputs(a.cfg),
			"terminal_log": string(logData),
		},
	})
	_, _ = io.WriteString(w, "data: "+string(hello)+"\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-q:
			if !ok {
				return
			}
			_, _ = io.WriteString(w, "data: "+msg+"\n\n")
			flusher.Flush()
		case <-keepalive.C:
			_, _ = io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (a *App) serveFile(w http.ResponseWriter, r *http.Request, path string, download bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		a.send(w, http.StatusNotFound, []byte(`{"error":"not found"}`), "application/json", nil)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		a.send(w, http.StatusNotFound, []byte(`{"error":"not found"}`), "application/json", nil)
		return
	}
	ctype := mime.TypeByExtension(filepath.Ext(path))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	extra := map[string]string{"Accept-Ranges": "bytes"}
	if download {
		extra["Content-Disposition"] = fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(path))
	}
	if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") {
		startS, endS, ok := parseRange(strings.TrimPrefix(rng, "bytes="), len(data))
		if ok {
			part := data[startS : endS+1]
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", startS, endS, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(len(part)))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(part)
			return
		}
	}
	a.send(w, http.StatusOK, data, ctype, extra)
}

func parseRange(spec string, size int) (int, int, bool) {
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start := 0
	end := size - 1
	var err error
	if parts[0] != "" {
		start, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, 0, false
		}
	}
	if parts[1] != "" {
		end, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, false
		}
	}
	if start < 0 || start >= size {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	if end < start {
		return 0, 0, false
	}
	return start, end, true
}

func (a *App) send(w http.ResponseWriter, code int, body []byte, ctype string, extra map[string]string) {
	for k, v := range extra {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func (a *App) json(w http.ResponseWriter, obj any) {
	a.jsonCode(w, obj, http.StatusOK)
}

func (a *App) jsonCode(w http.ResponseWriter, obj any, code int) {
	data, _ := json.Marshal(obj)
	a.send(w, code, data, "application/json", nil)
}
