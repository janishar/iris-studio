package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// The prompt upsampler, a Python subprocess that writes the caption FLUX.3 is
// trained on. It is optional and stays that way: the studio builds, starts and
// renders with no Python here, and nothing below runs unless a job asks for it.
const (
	upsampleStage    = "Upsampling"
	maxRefsUpsample  = 10   // MAX_REFERENCES of diffusers' upsampler block
	maxCaptionTokens = 1280 // iris.c, QWEN3_VL_MAX_SEQ_LEN, which truncates the rest
)

// pupFolders are the names a pup checkpoint has beside its image model:
// helmstudio's, from the repo, and the short one.
var pupFolders = []string{"flux-3-image-klein-pup", "flux-3-klein-pup"}

// pupRequest is what pup/upsample.py reads on stdin.
type pupRequest struct {
	Model       string          `json:"model"`
	TextEncoder string          `json:"text_encoder"`
	Prompt      string          `json:"prompt"`
	References  []string        `json:"references"`
	Boxes       json.RawMessage `json:"boxes,omitempty"`
	Width       int             `json:"width"`
	Height      int             `json:"height"`
}

// pupAnswer is what it writes on stdout, and what the caption cache keeps.
type pupAnswer struct {
	Caption string `json:"caption"`
	Tokens  int    `json:"tokens"`
}

func wantsUpsample(p map[string]any) bool {
	return boolFrom(p["upsample"]) || boolFrom(p["upsample_only"])
}

// boxesEmpty is a form with no layout boxes, typed or otherwise.
func boxesEmpty(raw any) bool {
	text, isText := raw.(string)
	return raw == nil || (isText && stringsTrimSpace(text) == "")
}

// checkBoxes is the reference's shape check, and "" when it passes: the
// coordinates are upsample.py's to refuse, when the job runs.
func checkBoxes(raw any) string {
	if boxesEmpty(raw) {
		return ""
	}
	var boxes []map[string]any
	text, isText := raw.(string)
	if !isText || !json.Valid([]byte(text)) {
		return "Layout boxes must be JSON."
	}
	shape := `Layout boxes must be a non-empty list of objects, each with a string "id".`
	if json.Unmarshal([]byte(text), &boxes) != nil || len(boxes) == 0 {
		return shape
	}
	for _, box := range boxes {
		if _, ok := box["id"].(string); !ok {
			return shape
		}
	}
	return ""
}

// isPupDir reports whether path looks like the upsampler's checkpoint.
func isPupDir(path string) bool {
	return FileExists(filepath.Join(path, "config.json")) && FileExists(filepath.Join(path, "system_prompts.json"))
}

// pupCheckpoint is the checkpoint chosen in Paths, else one beside the image
// model, else "".
func (c *Config) pupCheckpoint() string {
	candidates := []string{c.Pup}
	for _, name := range pupFolders {
		candidates = append(candidates, filepath.Join(filepath.Dir(c.Model), name))
	}
	for _, path := range candidates {
		if path != "" && isPupDir(path) {
			return path
		}
	}
	return ""
}

func (c *Config) pupPython() string {
	return filepath.Join(c.PupDir, ".venv", "bin", "python")
}

// upsampleUnavailable says why Upsample cannot be used here, or "" when it can.
func (c *Config) upsampleUnavailable() string {
	switch {
	case modelFamily(c.Model) != "flux3":
		return "The prompt upsampler is for FLUX.3 Image Klein, and this checkpoint is not one."
	case !FileExists(c.pupPython()):
		return "The prompt upsampler's Python environment is missing: run uv sync --locked in " + c.PupDir + "."
	case c.pupCheckpoint() == "":
		return "No prompt upsampler checkpoint was found beside " + c.Model + ". Choose one in Paths."
	}
	return ""
}

// upsampleStatus is what /api/config and the Paths routes say about it.
func (c *Config) upsampleStatus() map[string]any {
	reason := c.upsampleUnavailable()
	return map[string]any{
		"pup":                c.pupCheckpoint(),
		"upsample_available": reason == "",
		"upsample_reason":    reason,
		"max_refs_upsample":  maxRefsUpsample,
	}
}

// replPrompt keeps text that would be taken for something else out of the two
// places iris looks, on a REPL line with no size in front of it: "$" at the
// start reads as a reference, and a size at either end as a size. validate
// refuses a typed prompt that does this; a caption is not there to reword, so
// it gets the words and the full stop that end both readings.
func replPrompt(prompt string) string {
	if strings.HasPrefix(prompt, "$") || readsAsSize(prompt) {
		return "Image: " + prompt + "."
	}
	return prompt
}

// captionPath is the caption cache's file for this request, one per hash in
// the session's directory. Generation is greedy, so the same request always
// writes the same caption, and a new seed need not load 19 GB to get it.
func captionPath(session string, req pupRequest) (string, error) {
	sum := sha256.New()
	fmt.Fprintf(sum, "%q %q %d %d\n", req.Prompt, req.Boxes, req.Width, req.Height)
	for _, path := range req.References {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		image := sha256.Sum256(data)
		sum.Write(image[:])
	}
	return filepath.Join(session, "captions", hex.EncodeToString(sum.Sum(nil))+".json"), nil
}

// upsample is a job's first stage, and says whether the render goes on. A job
// that needs no caption passes straight through it.
func (r *Runner) upsample(job *Job) bool {
	if !wantsUpsample(job.Params) {
		job.set(func(j *Job) { j.Stages[upsampleStage] = stage{State: "skip"} })
		return true
	}
	job.set(func(j *Job) {
		j.Phase = upsampleStage
		j.Stages.enter(upsampleStage, nowSeconds())
	})
	r.Emit("job", job.Summary())
	r.Emit("queue", r.QueueState())
	answer, err := r.caption(job)
	if err != nil {
		message := err.Error()
		job.set(func(j *Job) {
			if j.State == "cancelling" {
				j.State = "cancelled"
				return
			}
			j.State, j.Error = "failed", &message
		})
		return false
	}
	r.Emit("terminal", map[string]any{"line": fmt.Sprintf("[studio] caption: %d tokens", answer.Tokens),
		"running": true, "session": job.Params["session_name"]})
	carryOn := false
	job.set(func(j *Job) {
		j.Stages.leave("done", -1, nowSeconds())
		j.caption, j.tokens = answer.Caption, answer.Tokens
		switch {
		// iris is not running yet, so this is where a cancel takes effect.
		case j.State == "cancelling":
			j.State = "cancelled"
		case boolFrom(j.Params["upsample_only"]):
			j.State = "done"
		case answer.Tokens > maxCaptionTokens:
			message := fmt.Sprintf("The caption came to %d tokens and iris reads %d, so the end of it would be lost. Shorten the prompt or use fewer layout boxes.",
				answer.Tokens, maxCaptionTokens)
			j.State, j.Error = "failed", &message
		default:
			carryOn = true
		}
	})
	r.Emit("job", job.Summary())
	return carryOn
}

// caption is this job's caption, from the session's cache or from a run of pup,
// which then fills it.
func (r *Runner) caption(job *Job) (pupAnswer, error) {
	p := job.Params
	var answer pupAnswer
	inputs, _, err := r.cfg.SessionDirs(anyToString(p["session_name"]))
	if err != nil {
		return answer, err
	}
	plan := planSize(r.cfg, p)
	req := pupRequest{
		Model:       r.cfg.pupCheckpoint(),
		TextEncoder: filepath.Join(r.cfg.Model, "text_encoder"),
		Prompt:      stringsTrimSpace(anyToString(p["prompt"])),
		References:  []string{},
		Width:       plan.Width,
		Height:      plan.Height,
	}
	if !boxesEmpty(p["boxes"]) {
		req.Boxes = json.RawMessage(stringsTrimSpace(anyToString(p["boxes"])))
	}
	refs, _ := p["refs"].([]any)
	for _, raw := range refs {
		if ref, ok := raw.(map[string]any); ok {
			req.References = append(req.References, filepath.Join(inputs, anyToString(ref["name"])))
		}
	}
	path, err := captionPath(filepath.Dir(inputs), req)
	if err != nil {
		return answer, err
	}
	if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &answer) == nil && answer.Caption != "" {
		return answer, nil
	}
	if answer, err = r.runPup(job, req); err != nil {
		return answer, err
	}
	// A cache that cannot be written costs the next seed a caption, no more.
	if os.MkdirAll(filepath.Dir(path), 0o755) == nil {
		_ = WriteJSONFile(path, answer, true)
	}
	return answer, nil
}

// runPup runs the upsampler to a caption. It has the process group and the
// r.proc iris gets in run, so Cancel stops it the same way. r.proc is set after
// Start for run's reason: until then there is no process to stop, and the check
// behind it catches a cancel that came first.
func (r *Runner) runPup(job *Job, req pupRequest) (pupAnswer, error) {
	var answer pupAnswer
	if reason := r.cfg.upsampleUnavailable(); reason != "" {
		return answer, errors.New(reason)
	}
	// Its 21 GB and the REPL's transformer do not fit together. The REPL loads
	// again when the render reaches it.
	if r.interactiveLoaded() {
		r.Emit("terminal", map[string]any{"line": "[studio] unloading interactive iris for the upsampler",
			"running": true, "session": job.Params["session_name"]})
		r.StopInteractive()
	}
	stdin, err := json.Marshal(req)
	if err != nil {
		return answer, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return answer, err
	}
	var stdout bytes.Buffer
	cmd := exec.Command(r.cfg.pupPython(), filepath.Join(r.cfg.PupDir, "upsample.py"))
	cmd.Dir = r.cfg.PupDir
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = &stdout
	cmd.Stderr = writer
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	job.set(func(j *Job) { j.Command = append([]string{}, cmd.Args...) })
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return answer, err
	}
	_ = writer.Close()
	r.mu.Lock()
	r.proc = cmd
	r.mu.Unlock()
	if job.state() == "cancelling" {
		go stopProcess(cmd)
	}
	r.pump(job, reader)
	_ = reader.Close()
	waited := cmd.Wait()
	r.mu.Lock()
	r.proc = nil
	r.mu.Unlock()
	if waited != nil {
		var message string
		job.set(func(j *Job) { message = j.lastError })
		return answer, errors.New(firstString(message, "the prompt upsampler stopped: "+waited.Error()))
	}
	if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil || answer.Caption == "" {
		return answer, errors.New("the prompt upsampler wrote no caption")
	}
	return answer, nil
}
