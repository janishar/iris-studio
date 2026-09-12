package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var stemRE = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

const (
	minDimension = 64
	maxDimension = 1792
	dimStep      = 16
	maxRefs      = 16
)

func safeStem(text string) string {
	stem := stemRE.ReplaceAllString(text, "-")
	stem = strings.Trim(stem, "-")
	stem = strings.ToLower(stem)
	if stem == "" {
		stem = "take"
	}
	if len(stem) > 48 {
		stem = stem[:48]
	}
	return stem
}

func defaultSessionSettings(name string) map[string]any {
	return map[string]any{
		"session_name": safeStem(name),
		"label":        "",
		"prompt":       "",
		"width":        maxDimension,
		"height":       maxDimension,
		"steps":        0,  // 0 = auto (model-dependent)
		"seed":         -1, // -1 = random
		"guidance":     0,  // 0 = auto (model-dependent)
		"schedule":     "default",
		"power_alpha":  2.0,
		"base_mode":    false,
		"mmap":         true,
		"run_mode":     "oneshot", // oneshot | interactive
		"show_steps":   false,
		"zoom":         2,
		"refs":         []any{},
		"takes":        []any{},
	}
}

func writeSidecar(outPath string, job *Job) {
	meta := job.Summary()
	delete(meta, "log")
	if job.Started != nil && job.Finished != nil {
		meta["duration_s"] = round2(*job.Finished - *job.Started)
	}
	_ = WriteJSONFile(strings.TrimSuffix(outPath, filepath.Ext(outPath))+".json", meta, false)
}

func saveSession(cfg *Config, params map[string]any) (string, error) {
	name := safeStem(anyToString(firstNonEmpty(params["session_name"], params["label"], "session-1")))
	path := cfg.SessionSetting(name)
	cloned := cloneMap(params)
	cloned["session_name"] = name
	existing := readJSONObject(path)
	takes, ok := existing["takes"].([]any)
	if !ok {
		takes = []any{}
	}
	cloned["takes"] = takes
	if err := WriteJSONFile(path, cloned, true); err != nil {
		return "", err
	}
	return name, nil
}

func recordTake(cfg *Config, job *Job) {
	name := safeStem(anyToString(firstNonEmpty(job.Params["session_name"], "session-1")))
	path := cfg.SessionSetting(name)
	data := readJSONObject(path)
	takes, ok := data["takes"].([]any)
	if !ok {
		takes = []any{}
	}
	entry := job.Summary()
	delete(entry, "log")
	if job.Started != nil && job.Finished != nil {
		entry["duration_s"] = round2(*job.Finished - *job.Started)
	}
	takes = append(takes, entry)
	data["session_name"] = name
	data["takes"] = takes
	_ = WriteJSONFile(path, data, true)
}

func pruneTake(cfg *Config, outputName string) {
	path := cfg.SessionSetting(cfg.CurrentSession())
	settings := readJSONObject(path)
	takes, ok := settings["takes"].([]any)
	if !ok {
		return
	}
	filtered := make([]any, 0, len(takes))
	for _, raw := range takes {
		take, ok := raw.(map[string]any)
		if ok && anyToString(take["output"]) == outputName {
			continue
		}
		filtered = append(filtered, raw)
	}
	settings["takes"] = filtered
	_ = WriteJSONFile(path, settings, true)
}

func deleteSession(cfg *Config, name string) (string, error) {
	session := safeStem(name)
	root := filepath.Join(cfg.Sessions, session)
	if !DirExists(root) {
		return "", errors.New("session not found")
	}
	if err := os.RemoveAll(root); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(cfg.Sessions)
	if err != nil {
		return session, nil
	}
	remaining := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if FileExists(filepath.Join(cfg.Sessions, entry.Name(), "setting.json")) {
			remaining = append(remaining, entry.Name())
		}
	}
	sort.Strings(remaining)
	if len(remaining) == 0 {
		return session, nil
	}
	idx := sort.SearchStrings(remaining, session)
	if idx > 0 {
		return remaining[idx-1], nil
	}
	return remaining[0], nil
}

func duplicateSession(cfg *Config, srcName, dstName string) (string, error) {
	src := safeStem(srcName)
	srcRoot := filepath.Join(cfg.Sessions, src)
	if !DirExists(srcRoot) {
		return "", errors.New("session not found")
	}
	var dst string
	if stringsTrimSpace(dstName) == "" {
		dst = src + "-copy"
		for i := 2; DirExists(filepath.Join(cfg.Sessions, dst)); i++ {
			dst = fmt.Sprintf("%s-copy-%d", src, i)
		}
	} else {
		dst = safeStem(dstName)
		if DirExists(filepath.Join(cfg.Sessions, dst)) {
			return "", errors.New("a session with that name already exists")
		}
	}
	dstRoot := filepath.Join(cfg.Sessions, dst)
	if err := copyDir(srcRoot, dstRoot); err != nil {
		_ = os.RemoveAll(dstRoot)
		return "", err
	}
	settingPath := filepath.Join(dstRoot, "setting.json")
	if data := readJSONObject(settingPath); len(data) > 0 {
		data["session_name"] = dst
		_ = WriteJSONFile(settingPath, data, true)
	}
	_ = os.WriteFile(filepath.Join(dstRoot, "terminal.log"), []byte{}, 0o644)
	return dst, nil
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func listSessions(cfg *Config) []map[string]any {
	entries, err := os.ReadDir(cfg.Sessions)
	if err != nil {
		return []map[string]any{}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := make([]map[string]any, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(cfg.Sessions, entry.Name(), "setting.json")
		if !FileExists(path) {
			continue
		}
		data := readJSONObject(path)
		if len(data) == 0 {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{
			"name":   entry.Name(),
			"params": data,
			"mtime":  float64(info.ModTime().UnixNano()) / 1e9,
		})
	}
	return out
}

// listInputs lists the reference images uploaded into the active session's
// inputs/ directory. Iris only accepts image references (PNG/JPEG/PPM).
func listInputs(cfg *Config) []map[string]any {
	exts := map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".ppm": true}
	entries, err := os.ReadDir(cfg.CurrentInputs())
	if err != nil {
		return []map[string]any{}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	items := make([]map[string]any, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		suffix := strings.ToLower(filepath.Ext(entry.Name()))
		if !exts[suffix] {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, map[string]any{
			"name": entry.Name(),
			"kind": "image",
			"size": info.Size(),
		})
	}
	return items
}

func listOutputs(cfg *Config) []map[string]any {
	return listImageDir(cfg.CurrentOutputs(), 200)
}

// listImageDir lists *.png files in dir, newest first, each with its sidecar
// .json metadata (if any). limit caps the number of entries (0 = no cap).
func listImageDir(dir string, limit int) []map[string]any {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.png"))
	sort.Slice(matches, func(i, j int) bool {
		ai, aerr := os.Stat(matches[i])
		bi, berr := os.Stat(matches[j])
		if aerr != nil || berr != nil {
			return matches[i] > matches[j]
		}
		return ai.ModTime().After(bi.ModTime())
	})
	items := make([]map[string]any, 0, len(matches))
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		meta := map[string]any{}
		side := strings.TrimSuffix(path, filepath.Ext(path)) + ".json"
		if FileExists(side) {
			meta = readJSONObject(side)
			if meta == nil {
				meta = map[string]any{}
			}
		}
		items = append(items, map[string]any{
			"name":  filepath.Base(path),
			"size":  info.Size(),
			"mtime": float64(info.ModTime().UnixNano()) / 1e9,
			"meta":  meta,
		})
		if limit > 0 && len(items) == limit {
			break
		}
	}
	return items
}

// importOutputAsInput copies a rendered take from outputs/ into inputs/ so it
// can be attached as a reference for a follow-up generation.
func importOutputAsInput(cfg *Config, name string) (string, error) {
	src := filepath.Join(cfg.CurrentOutputs(), filepath.Base(name))
	if !FileExists(src) {
		return "", os.ErrNotExist
	}
	ext := filepath.Ext(src)
	base := strings.TrimSuffix(filepath.Base(src), ext)
	dst := filepath.Join(cfg.CurrentInputs(), base+ext)
	for i := 1; FileExists(dst); i++ {
		dst = filepath.Join(cfg.CurrentInputs(), fmt.Sprintf("%s-%d%s", base, i, ext))
	}
	if err := copyFile(src, dst); err != nil {
		return "", err
	}
	return filepath.Base(dst), nil
}

func validate(cfg *Config, params map[string]any) []string {
	errs := []string{}
	w, h := intFrom(params["width"], 0), intFrom(params["height"], 0)
	if w%dimStep != 0 || h%dimStep != 0 {
		errs = append(errs, fmt.Sprintf("Width and height must be multiples of %d.", dimStep))
	}
	if w < minDimension || h < minDimension {
		errs = append(errs, fmt.Sprintf("Width and height must be at least %d.", minDimension))
	}
	if w > maxDimension || h > maxDimension {
		errs = append(errs, fmt.Sprintf("Width and height must be at most %d.", maxDimension))
	}
	if stringsTrimSpace(anyToString(params["prompt"])) == "" {
		errs = append(errs, "Write a prompt.")
	}
	if !isModelDir(cfg.Model) {
		errs = append(errs, fmt.Sprintf("The configured model directory doesn't look valid: %s. Change it in Paths.", cfg.Model))
	}
	refs, ok := params["refs"].([]any)
	if !ok {
		if params["refs"] != nil {
			errs = append(errs, "References must be an ordered list.")
		}
		refs = []any{}
	}
	if len(refs) > maxRefs {
		errs = append(errs, fmt.Sprintf("At most %d reference images.", maxRefs))
	}
	for _, raw := range refs {
		ref, ok := raw.(map[string]any)
		if !ok || stringsTrimSpace(anyToString(ref["name"])) == "" {
			errs = append(errs, "Each reference must name an uploaded image.")
		}
	}
	return errs
}

func WriteJSONFile(path string, obj any, newline bool) error {
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	if newline {
		data = append(data, '\n')
	}
	return os.WriteFile(path, data, 0o644)
}

func readJSONObject(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func readStringField(path, field string) (string, bool) {
	obj := readJSONObject(path)
	value := stringsTrimSpace(anyToString(obj[field]))
	return value, value != ""
}

func cloneMap(src map[string]any) map[string]any {
	data, err := json.Marshal(src)
	if err != nil {
		out := make(map[string]any, len(src))
		for k, v := range src {
			out[k] = v
		}
		return out
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func anyToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case fmt.Stringer:
		return t.String()
	case float64:
		if math.Trunc(t) == t {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func firstNonEmpty(values ...any) any {
	for _, v := range values {
		if stringsTrimSpace(anyToString(v)) != "" {
			return v
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values[len(values)-1]
}

func firstString(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

func intFrom(v any, fallback int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case float32:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		i, err := t.Int64()
		if err == nil {
			return int(i)
		}
		f, err := t.Float64()
		if err == nil {
			return int(f)
		}
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(t))
		if err == nil {
			return i
		}
	}
	return fallback
}

func int64From(v any, fallback int64) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case json.Number:
		i, err := t.Int64()
		if err == nil {
			return i
		}
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err == nil {
			return i
		}
	}
	return fallback
}

func floatFrom(v any, fallback float64) float64 {
	f, err := floatFromStrict(v)
	if err != nil || v == nil {
		return fallback
	}
	return f
}

func boolFromDefault(v any, def bool) bool {
	if v == nil {
		return def
	}
	return boolFrom(v)
}

func boolFrom(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(t))
		return err == nil && parsed
	case float64:
		return t != 0
	case int:
		return t != 0
	default:
		return false
	}
}

func floatFromStrict(v any) (float64, error) {
	if v == nil {
		return 0, nil
	}
	switch t := v.(type) {
	case float64:
		return t, nil
	case float32:
		return float64(t), nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case json.Number:
		return t.Float64()
	case string:
		if strings.TrimSpace(t) == "" {
			return 0, nil
		}
		return strconv.ParseFloat(strings.TrimSpace(t), 64)
	default:
		return 0, errors.New("invalid float")
	}
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func FileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func DirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func ensureFile(path string) error {
	if FileExists(path) {
		return nil
	}
	return os.WriteFile(path, []byte{}, 0o644)
}

func expandHome(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

func stringsTrimSpace(s string) string { return strings.TrimSpace(s) }

func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }
