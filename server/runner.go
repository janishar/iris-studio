package server

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A render, queued and then run.
//
// Everything below mu is written by the one goroutine that runs the render and
// read by whichever HTTP goroutine asks what the queue is doing, so every one
// of them goes through set and Summary. What sits above mu is written once, by
// newJob, and is read without it.
type Job struct {
	ID     string
	Params map[string]any
	Label  string

	mu       sync.Mutex
	State    string
	Phase    string
	Progress []int
	Log      []string
	Command  []string
	Output   *string
	Seed     *int64
	Error    *string
	Started  *float64
	Finished *float64

	// OutputPath is where Output actually is on disk. The page is given the
	// name; helmstudio is given the path, because adopting a take is a
	// hardlink and a hardlink needs the file.
	OutputPath string
	// task is this render, mirrored to helmstudio for as long as it runs, or
	// nil when there is no platform or it opened no job. Every method on it
	// is a no-op when it is nil, so nothing below asks whether there is one.
	task *Task
}

func newJob(params map[string]any) *Job {
	return &Job{
		ID:      randomID(),
		Params:  cloneMap(params),
		Label:   anyToString(params["label"]),
		State:   "queued",
		Log:     []string{},
		Command: []string{},
	}
}

// set is the only way a field below mu is written. It takes no lock of the
// runner's, and nothing inside fn may: a job is always locked after the runner,
// never before it.
func (j *Job) set(fn func(j *Job)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	fn(j)
}

// fail ends the job with a reason. It is the shape every failing path here
// had already written out by hand, which is why there were so many of them.
func (j *Job) fail(message string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.State = "failed"
	j.Error = &message
}

// state is the job's state on its own, for a caller that wants nothing else.
func (j *Job) state() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.State
}

// outcome is the state a finished job ended in and what went wrong, read
// together so the two cannot disagree.
func (j *Job) outcome() (string, string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Error == nil {
		return j.State, ""
	}
	return j.State, *j.Error
}

// outputPath is where the take was written, or "" when there was none.
func (j *Job) outputPath() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.OutputPath
}

// helm is the helmstudio job this render is mirrored on, or nil. Every method
// on the result is a no-op on nil, so a caller never asks whether there is one.
func (j *Job) helm() *Task {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.task
}

// duration is how long the render took, and whether it ran at all.
func (j *Job) duration() (float64, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Started == nil || j.Finished == nil {
		return 0, false
	}
	return round2(*j.Finished - *j.Started), true
}

// Summary is what the page and the sidecar are told about a job. The slices
// and the map are copies: what it returns outlives the lock, and the render
// goes on appending to the originals.
func (j *Job) Summary() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	var output any
	if j.Output != nil {
		output = *j.Output
	}
	var seed any
	if j.Seed != nil {
		seed = *j.Seed
	}
	var errValue any
	if j.Error != nil {
		errValue = *j.Error
	}
	var started any
	if j.Started != nil {
		started = *j.Started
	}
	var finished any
	if j.Finished != nil {
		finished = *j.Finished
	}
	log := j.Log
	if len(log) > 400 {
		log = log[len(log)-400:]
	}
	summary := map[string]any{
		"id":       j.ID,
		"label":    j.Label,
		"state":    j.State,
		"phase":    j.Phase,
		"progress": append([]int(nil), j.Progress...),
		"output":   output,
		"seed":     seed,
		"error":    errValue,
		"started":  started,
		"finished": finished,
		// Shared, not copied: Params is written once by newJob and never
		// again, and this runs once per line of a render's output.
		"params":  j.Params,
		"command": append([]string(nil), j.Command...),
		"log":     append([]string(nil), log...),
	}
	// Left out rather than sent empty: no job means there is nothing to
	// stream, and the page reads its absence as exactly that.
	if id := j.task.JobID(); id != "" {
		summary["helm_job"] = id
	}
	return summary
}

// interactiveState tracks the toggle-style settings of the currently loaded
// interactive `iris` REPL process. iris_cli.c's !linear/!power/!sigmoid/
// !flowmatch/!show-steps commands each *flip* a piece of state rather than
// setting it absolutely, so the runner must remember what it last set in
// order to know whether (and how) to change it.
type interactiveState struct {
	schedule   string // "default" | "linear" | "power" | "sigmoid" | "flowmatch"
	powerAlpha float64
	showSteps  bool
	zoom       int
	refIDs     map[string]int // session-input path -> $N assigned by iris
	nextRefID  int
	tmpDir     string // iris's own mkdtemp output dir for this process, discovered lazily
}

func newInteractiveState() *interactiveState {
	return &interactiveState{schedule: "default", powerAlpha: 2.0, zoom: 2, refIDs: map[string]int{}}
}

func (st *interactiveState) syncSchedule(desired string, desiredAlpha float64) []string {
	if desiredAlpha <= 0 {
		desiredAlpha = 2.0
	}
	var cmds []string
	toggle := func(name string) string {
		switch name {
		case "linear":
			return "!linear"
		case "sigmoid":
			return "!sigmoid"
		case "flowmatch":
			return "!flowmatch"
		}
		return ""
	}
	if st.schedule == "power" && desired == "power" && st.powerAlpha != desiredAlpha {
		cmds = append(cmds, "!power") // toggle off first so the next !power re-enables with a fresh alpha
		st.schedule = "default"
	}
	if st.schedule != desired {
		switch desired {
		case "linear", "sigmoid", "flowmatch":
			cmds = append(cmds, toggle(desired))
		case "power":
			cmds = append(cmds, fmt.Sprintf("!power %.3f", desiredAlpha))
		default: // "default": clear whatever is currently active
			switch st.schedule {
			case "linear", "sigmoid", "flowmatch":
				cmds = append(cmds, toggle(st.schedule))
			case "power":
				cmds = append(cmds, "!power")
			}
		}
		st.schedule = desired
	}
	st.powerAlpha = desiredAlpha
	return cmds
}

func (st *interactiveState) syncShowSteps(desired bool) []string {
	if st.showSteps == desired {
		return nil
	}
	st.showSteps = desired
	return []string{"!show-steps"}
}

func (st *interactiveState) syncZoom(desired int) []string {
	if desired <= 0 {
		desired = 2
	}
	if st.zoom == desired {
		return nil
	}
	st.zoom = desired
	return []string{fmt.Sprintf("!zoom %d", desired)}
}

type Runner struct {
	cfg *Config

	mu      sync.Mutex
	jobs    map[string]*Job
	order   []string
	current *Job
	proc    *exec.Cmd

	listeners []chan string
	queue     chan string

	interactiveLock  sync.Mutex
	interactiveProc  *exec.Cmd
	interactiveIn    io.WriteCloser
	interactiveDone  chan struct{}
	interactiveLines chan *string
	istate           *interactiveState

	terminalLock sync.Mutex
	terminalProc *exec.Cmd

	reloadMu      sync.Mutex
	reloadClients map[chan string]bool
}

func NewRunner(cfg *Config) *Runner {
	r := &Runner{
		cfg:              cfg,
		jobs:             map[string]*Job{},
		order:            []string{},
		listeners:        []chan string{},
		queue:            make(chan string, 200),
		interactiveLines: make(chan *string, 1000),
	}
	go r.loop()
	return r
}

func (r *Runner) ReloadClients() {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	for client := range r.reloadClients {
		select {
		case client <- "reload":
		default:
		}
	}
}

func (r *Runner) SubscribeReload() chan string {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	if r.reloadClients == nil {
		r.reloadClients = make(map[chan string]bool)
	}
	q := make(chan string, 10)
	r.reloadClients[q] = true
	return q
}

func (r *Runner) UnsubscribeReload(q chan string) {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	delete(r.reloadClients, q)
	close(q)
}

func (r *Runner) Subscribe() chan string {
	q := make(chan string, 200)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listeners = append(r.listeners, q)
	return q
}

func (r *Runner) Unsubscribe(q chan string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, listener := range r.listeners {
		if listener == q {
			r.listeners = append(r.listeners[:i], r.listeners[i+1:]...)
			close(listener)
			break
		}
	}
}

func (r *Runner) Emit(kind string, payload any) {
	if kind == "terminal" {
		if m, ok := payload.(map[string]any); ok {
			if line := anyToString(m["line"]); line != "" {
				path := r.cfg.TerminalLog("")
				f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err == nil {
					_, _ = f.WriteString(line + "\n")
					_ = f.Close()
				}
			}
		}
	}
	data, _ := json.Marshal(map[string]any{"kind": kind, "payload": payload})
	r.mu.Lock()
	defer r.mu.Unlock()
	alive := r.listeners[:0]
	for _, listener := range r.listeners {
		select {
		case listener <- string(data):
			alive = append(alive, listener)
		default:
			close(listener)
		}
	}
	r.listeners = alive
}

func (r *Runner) Submit(params map[string]any) *Job {
	_ = os.WriteFile(r.cfg.TerminalLog(anyToString(params["session_name"])), []byte{}, 0o644)
	job := newJob(params)
	r.mu.Lock()
	r.jobs[job.ID] = job
	r.order = append(r.order, job.ID)
	r.mu.Unlock()
	r.queue <- job.ID
	r.Emit("queue", r.QueueState())
	return job
}

func (r *Runner) Cancel(jobID string) bool {
	r.mu.Lock()
	job := r.jobs[jobID]
	proc := r.proc
	if job == nil {
		r.mu.Unlock()
		return false
	}
	switch job.state() {
	case "queued":
		job.set(func(j *Job) { j.State = "cancelled" })
		r.mu.Unlock()
		r.Emit("queue", r.QueueState())
		return true
	case "running":
		if proc != nil && proc.Process != nil {
			job.set(func(j *Job) { j.State = "cancelling" })
			r.mu.Unlock()
			go stopProcess(proc)
			return true
		}
	}
	r.mu.Unlock()
	return false
}

func stopProcess(proc *exec.Cmd) {
	if proc == nil || proc.Process == nil {
		return
	}
	_ = syscall.Kill(-proc.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_, _ = proc.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(2 * time.Second):
	}
	_ = syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
}

func (r *Runner) RunTerminal(command string) bool {
	r.terminalLock.Lock()
	defer r.terminalLock.Unlock()
	r.mu.Lock()
	busy := r.current != nil
	r.mu.Unlock()
	if r.terminalProc != nil || busy {
		return false
	}
	go r.runTerminal(command)
	return true
}

func (r *Runner) runTerminal(command string) {
	r.Emit("terminal", map[string]any{"running": true})
	reader, writer, err := os.Pipe()
	if err != nil {
		r.Emit("terminal", map[string]any{"line": err.Error(), "running": false})
		return
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = r.cfg.Workdir
	cmd.Stdout = writer
	cmd.Stderr = writer
	r.terminalLock.Lock()
	r.terminalProc = cmd
	r.terminalLock.Unlock()
	startErr := cmd.Start()
	_ = writer.Close()
	if startErr != nil {
		_ = reader.Close()
		r.Emit("terminal", map[string]any{"line": startErr.Error(), "running": false})
		r.terminalLock.Lock()
		r.terminalProc = nil
		r.terminalLock.Unlock()
		return
	}
	buf := bufio.NewScanner(reader)
	buf.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for buf.Scan() {
		line := strings.TrimRight(buf.Text(), " \t\r\n")
		if line != "" {
			r.Emit("terminal", map[string]any{"line": line, "running": true})
		}
	}
	_ = reader.Close()
	code := 0
	if err := cmd.Wait(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			code = 1
		}
	}
	r.Emit("terminal", map[string]any{"line": fmt.Sprintf("[exit %d]", code), "running": false})
	r.terminalLock.Lock()
	r.terminalProc = nil
	r.terminalLock.Unlock()
}

func (r *Runner) QueueState() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []map[string]any{}
	for _, id := range r.order {
		job := r.jobs[id]
		if job == nil {
			continue
		}
		if state := job.state(); state == "queued" || state == "running" {
			out = append(out, job.Summary())
		}
	}
	return out
}

func (r *Runner) History(limit int) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []map[string]any{}
	for i := len(r.order) - 1; i >= 0; i-- {
		job := r.jobs[r.order[i]]
		if job == nil {
			continue
		}
		if state := job.state(); state == "done" || state == "failed" || state == "cancelled" {
			out = append(out, job.Summary())
			if len(out) == limit {
				break
			}
		}
	}
	return out
}

func (r *Runner) loop() {
	for jobID := range r.queue {
		r.mu.Lock()
		job := r.jobs[jobID]
		if job == nil || job.state() == "cancelled" {
			r.mu.Unlock()
			continue
		}
		r.current = job
		r.mu.Unlock()
		// Reported to helmstudio for as long as it runs. Opened here rather
		// than inside the run: it talks to the platform, and no lock of iris
		// studio's is held while it does.
		if task := r.cfg.Platform.StartTask(); task != nil {
			job.set(func(j *Job) { j.task = task })
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					msg := fmt.Sprint(rec)
					job.set(func(j *Job) { j.State = "failed"; j.Error = &msg })
				}
				now := nowSeconds()
				job.set(func(j *Job) { j.Finished = &now })
				r.mu.Lock()
				r.current = nil
				r.proc = nil
				r.mu.Unlock()
				recordTake(r.cfg, job)
				// Read together, so the state the job ended in and the reason
				// it gives cannot come from two different moments.
				state, failure := job.outcome()
				if state == "done" {
					_, _ = saveSession(r.cfg, job.Params)
					// The take is on disk and recorded here; helmstudio gets
					// it too. Never fatal: a platform that refuses costs a
					// line in the log, not the generation.
					r.cfg.Platform.RecordTake(job.outputPath(), job.Label,
						anyToString(job.Params["session_name"]), job.Summary())
				}
				// After the state is final, so helmstudio's job ends in the
				// state this one ended in, and its log ends where this ends.
				job.helm().Finish(state, failure)
				r.Emit("job", job.Summary())
				r.Emit("queue", r.QueueState())
				r.Emit("outputs", listOutputs(r.cfg))
			}()
			if anyToString(job.Params["run_mode"]) == "interactive" {
				r.runInteractive(job)
			} else {
				r.run(job)
			}
		}()
	}
}

// buildOneshotArgs translates session params into iris's one-shot CLI flags.
func buildOneshotArgs(cfg *Config, p map[string]any, inputs, outPath string, showSteps bool) []string {
	args := []string{cfg.Iris, "-d", cfg.Model, "-p", anyToString(p["prompt"]), "-o", outPath}
	args = append(args, "-W", anyToString(intFrom(p["width"], 512)), "-H", anyToString(intFrom(p["height"], 512)))
	if steps := intFrom(p["steps"], 0); steps > 0 {
		args = append(args, "-s", anyToString(steps))
	}
	seed := int64From(p["seed"], -1)
	if seed >= 0 {
		args = append(args, "-S", anyToString(seed))
	}
	if guidance := floatFrom(p["guidance"], 0); guidance > 0 {
		args = append(args, "-g", fmt.Sprintf("%.3f", guidance))
	}
	switch anyToString(p["schedule"]) {
	case "linear":
		args = append(args, "--linear")
	case "power":
		args = append(args, "--power")
		if alpha := floatFrom(p["power_alpha"], 0); alpha > 0 {
			args = append(args, "--power-alpha", fmt.Sprintf("%.3f", alpha))
		}
	case "sigmoid":
		args = append(args, "--sigmoid")
	case "flowmatch":
		args = append(args, "--flowmatch")
	}
	if boolFrom(p["base_mode"]) {
		args = append(args, "--base")
	}
	if !boolFromDefault(p["mmap"], true) {
		args = append(args, "--no-mmap")
	}
	if refs, ok := p["refs"].([]any); ok {
		for _, raw := range refs {
			ref, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			args = append(args, "-i", filepath.Join(inputs, anyToString(ref["name"])))
		}
	}
	args = append(args, "-v", "--no-license-info")
	if showSteps {
		args = append(args, "--show-steps")
	}
	return args
}

func (r *Runner) run(job *Job) {
	p := job.Params
	inputs, outputs, err := r.cfg.SessionDirs(anyToString(p["session_name"]))
	if err != nil {
		job.fail(err.Error())
		return
	}
	if !isModelDir(r.cfg.Model) {
		job.fail(fmt.Sprintf("configured model directory not found: %s", r.cfg.Model))
		return
	}
	stem := safeStem(firstString(anyToString(p["label"]), "take"))
	name := fmt.Sprintf("%s-%s.png", stem, time.Now().Format("0102-150405"))
	outPath := filepath.Join(outputs, name)
	showSteps := boolFrom(p["show_steps"])
	args := buildOneshotArgs(r.cfg, p, inputs, outPath, showSteps)
	now := nowSeconds()
	job.set(func(j *Job) {
		j.Command = append([]string{}, args...)
		j.State = "running"
		j.Started = &now
	})
	r.Emit("job", job.Summary())
	r.Emit("queue", r.QueueState())
	env := os.Environ()
	if showSteps {
		env = append(env, "KITTY_WINDOW_ID=1") // force Kitty terminal detection for --show-steps
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		job.fail(err.Error())
		return
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = r.cfg.Workdir
	cmd.Env = env
	cmd.Stdout = writer
	cmd.Stderr = writer
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r.mu.Lock()
	r.proc = cmd
	r.mu.Unlock()
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		job.fail(err.Error())
		return
	}
	_ = writer.Close()
	r.pump(job, reader)
	_ = reader.Close()
	code := 0
	if err := cmd.Wait(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			code = 1
		}
	}
	finished := nowSeconds()
	done := code == 0 && FileExists(outPath)
	cancelled := false
	job.set(func(j *Job) {
		j.Finished = &finished
		switch {
		case j.State == "cancelling":
			j.State = "cancelled"
			cancelled = true
		case done:
			j.State = "done"
			j.Output = &name
			j.OutputPath = outPath
		}
	})
	if cancelled {
		return
	}
	if done {
		// Outside the lock: the sidecar is written from the job's Summary,
		// which takes that same lock.
		writeSidecar(outPath, job)
		return
	}
	job.fail(fmt.Sprintf("iris exited with code %d", code))
}

var (
	seedRe     = regexp.MustCompile(`^Seed:\s*(-?\d+)\s*$`)
	stepMarkRe = regexp.MustCompile(`^\[Step (\d+)\]\s*$`)
	stepProgRe = regexp.MustCompile(`\[(\d+)/(\d+)\]:`)
	errorLnRe  = regexp.MustCompile(`^Error: (.*)$`)
	doneLnRe   = regexp.MustCompile(`^Done -> (.+?) \(ref \$(\d+)\)`)
	loadedLnRe = regexp.MustCompile(`\(ref \$(\d+)\)\s*$`)
)

// pump reads a one-shot iris process's combined stdout/stderr line by line,
// updating job progress and forwarding preview frames and raw log lines to
// subscribers in real time.
func (r *Runner) pump(job *Job, reader *os.File) {
	br := bufio.NewReader(reader)
	var buf []byte
	preview := newPreviewState()
	for {
		b, err := br.ReadByte()
		if err != nil {
			break
		}
		if b == '\n' || b == '\r' {
			line := strings.TrimRight(string(buf), " \t")
			buf = buf[:0]
			if line == "" {
				continue
			}
			r.handleLine(job, line, preview)
			continue
		}
		buf = append(buf, b)
	}
}

func (r *Runner) handleLine(job *Job, line string, preview *previewState) {
	// The same line the page and the session log get: a preview frame's
	// payload is megabytes of base64 and belongs in neither.
	text := truncateKittyLine(line)
	var num, den int
	job.set(func(j *Job) {
		j.Log = append(j.Log, text)
		if len(j.Log) > 400 {
			j.Log = j.Log[100:]
		}
		if m := seedRe.FindStringSubmatch(line); len(m) == 2 {
			if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				j.Seed = &v
			}
		}
		if m := stepProgRe.FindStringSubmatch(line); len(m) == 3 {
			j.Phase = "Denoising"
			num, den = intFrom(m[1], 0), intFrom(m[2], 0)
			j.Progress = []int{num, den}
		}
	})
	// Outside the lock: both take a lock of the task's, and a job is never
	// held while one is waited for. The two numbers travel rather than the
	// slice holding them, so nothing reads it after the lock is let go.
	task := job.helm()
	task.Log(text)
	task.Progress(num, den)
	if m := stepMarkRe.FindStringSubmatch(line); len(m) == 2 {
		preview.step = intFrom(m[1], 0)
	}
	if url, w, h, done := tryParsePreviewLine(line, preview); url != "" {
		r.Emit("preview", map[string]any{"url": url, "width": w, "height": h, "step": preview.step})
		_ = done
	}
	r.Emit("job", job.Summary())
	r.Emit("terminal", map[string]any{"line": text, "running": true})
}

// ---------------------------------------------------------------------------
// Interactive REPL mode
// ---------------------------------------------------------------------------

func (r *Runner) LoadInteractive(params map[string]any) (bool, string) {
	r.interactiveLock.Lock()
	defer r.interactiveLock.Unlock()
	r.normalizeInteractiveStateLocked()
	if r.interactiveProc != nil {
		return false, "interactive iris is already loaded"
	}
	if !isModelDir(r.cfg.Model) {
		return false, fmt.Sprintf("configured model directory not found: %s", r.cfg.Model)
	}
	name, err := saveSession(r.cfg, params)
	if err != nil {
		return false, err.Error()
	}
	params["session_name"] = name
	if _, _, err := r.cfg.ActivateSession(name); err != nil {
		return false, err.Error()
	}
	if err := r.ensureInteractiveLocked(); err != nil {
		return false, err.Error()
	}
	return true, ""
}

func (r *Runner) SendInteractive(line string) (bool, string) {
	text := stringsTrimSpace(line)
	if text == "" {
		return false, "input is required"
	}
	r.interactiveLock.Lock()
	defer r.interactiveLock.Unlock()
	r.normalizeInteractiveStateLocked()
	r.mu.Lock()
	busy := r.current != nil
	r.mu.Unlock()
	if busy {
		return false, "interactive iris is busy generating"
	}
	if r.interactiveProc == nil || r.interactiveIn == nil {
		return false, "load interactive iris first"
	}
	if err := r.interactiveSendLocked(text); err != nil {
		r.interactiveProc = nil
		r.interactiveIn = nil
		return false, "interactive iris has exited; load it again"
	}
	return true, ""
}

func (r *Runner) ensureInteractiveLocked() error {
	if r.interactiveProc != nil {
		return nil
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command(r.cfg.Iris, "-d", r.cfg.Model)
	cmd.Dir = r.cfg.Workdir
	cmd.Stdout = writer
	cmd.Stderr = writer
	cmd.Env = append(os.Environ(), "KITTY_WINDOW_ID=1") // force Kitty terminal detection for preview frames
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return err
	}
	spawnedAt := time.Now()
	if err := cmd.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		_ = stdin.Close()
		return err
	}
	_ = writer.Close()
	r.interactiveProc = cmd
	r.interactiveIn = stdin
	r.interactiveDone = make(chan struct{})
	r.istate = newInteractiveState()
	go r.readInteractive(reader, cmd, r.interactiveDone)
	if tmpDir, err := discoverIrisTmpDir(spawnedAt, 10*time.Second); err == nil {
		r.istate.tmpDir = tmpDir
	}
	return nil
}

func (r *Runner) readInteractive(reader *os.File, cmd *exec.Cmd, done chan struct{}) {
	defer close(done)
	defer reader.Close()
	br := bufio.NewReader(reader)
	var buf []byte
	preview := newPreviewState()
	for {
		b, err := br.ReadByte()
		if err != nil {
			break
		}
		if b == '\n' || b == '\r' {
			line := strings.TrimRight(string(buf), " \t")
			buf = buf[:0]
			if line != "" {
				text := line
				r.interactiveLines <- &text
				if m := stepMarkRe.FindStringSubmatch(line); len(m) == 2 {
					preview.step = intFrom(m[1], 0)
				}
				if url, w, h, done := tryParsePreviewLine(line, preview); url != "" {
					r.Emit("preview", map[string]any{"url": url, "width": w, "height": h, "step": preview.step})
					_ = done
				}
				r.Emit("terminal", map[string]any{"line": truncateKittyLine(line), "running": true})
			}
			continue
		}
		buf = append(buf, b)
	}
	_, _ = cmd.Process.Wait()
	r.interactiveLines <- nil
}

func (r *Runner) StopInteractive() {
	r.interactiveLock.Lock()
	defer r.interactiveLock.Unlock()
	proc := r.interactiveProc
	stdin := r.interactiveIn
	done := r.interactiveDone
	r.interactiveProc = nil
	r.interactiveIn = nil
	r.interactiveDone = nil
	if proc == nil {
		return
	}
	if stdin != nil {
		_, _ = io.WriteString(stdin, "!quit\n")
		_ = stdin.Close()
	}
	if done != nil {
		select {
		case <-done:
			return
		case <-time.After(5 * time.Second):
		}
	}
	if proc.Process != nil {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}
}

func (r *Runner) interactiveSendLocked(line string) error {
	if r.interactiveIn == nil {
		return fmt.Errorf("interactive stdin unavailable")
	}
	flat := strings.Join(strings.Fields(line), " ")
	if _, err := io.WriteString(r.interactiveIn, flat+"\n"); err != nil {
		return err
	}
	r.Emit("terminal", map[string]any{"line": "iris> " + flat, "running": true})
	return nil
}

func (r *Runner) normalizeInteractiveStateLocked() {
	if r.interactiveDone == nil {
		return
	}
	select {
	case <-r.interactiveDone:
		r.interactiveProc = nil
		r.interactiveIn = nil
		r.interactiveDone = nil
	default:
	}
}

func (r *Runner) runInteractive(job *Job) {
	p := job.Params
	refs, _ := p["refs"].([]any)
	now := nowSeconds()
	job.set(func(j *Job) { j.State = "running"; j.Started = &now })
	r.interactiveLock.Lock()
	defer r.interactiveLock.Unlock()
	if !isModelDir(r.cfg.Model) {
		job.fail(fmt.Sprintf("configured model directory not found: %s", r.cfg.Model))
		return
	}
	if err := r.ensureInteractiveLocked(); err != nil {
		job.fail(err.Error())
		return
	}
	inputs, outputs, err := r.cfg.SessionDirs(anyToString(p["session_name"]))
	if err != nil {
		job.fail(err.Error())
		return
	}

	var cmds []string
	cmds = append(cmds, fmt.Sprintf("!size %dx%d", intFrom(p["width"], 512), intFrom(p["height"], 512)))
	cmds = append(cmds, fmt.Sprintf("!steps %d", intFrom(p["steps"], 0)))
	cmds = append(cmds, fmt.Sprintf("!seed %d", int64From(p["seed"], -1)))
	cmds = append(cmds, fmt.Sprintf("!guidance %.3f", floatFrom(p["guidance"], 0)))
	cmds = append(cmds, r.istate.syncSchedule(anyToString(firstNonEmpty(p["schedule"], "default")), floatFrom(p["power_alpha"], 2.0))...)
	cmds = append(cmds, r.istate.syncShowSteps(boolFrom(p["show_steps"]))...)
	cmds = append(cmds, r.istate.syncZoom(intFrom(p["zoom"], 2))...)
	for _, cmd := range cmds {
		if err := r.interactiveSendLocked(cmd); err != nil {
			job.fail("interactive iris has exited; load it again")
			return
		}
	}

	// Resolve each reference to a $N token. iris_cli.c assigns ref IDs
	// sequentially (ref_add() increments a counter once per !load/generate),
	// and its own "Loaded: ..." confirmation is printed via unflushed
	// stdout buffering that a piped process may never see promptly — so
	// rather than waiting on that line, the runner tracks the same counter
	// itself and trusts the deterministic, single-threaded REPL ordering.
	var refTokens []string
	for _, raw := range refs {
		ref, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		path := filepath.Join(inputs, anyToString(ref["name"]))
		id, known := r.istate.refIDs[path]
		if !known {
			if err := r.interactiveSendLocked("!load " + path); err != nil {
				job.fail("interactive iris has exited; load it again")
				return
			}
			id = r.istate.nextRefID
			r.istate.nextRefID++
			r.istate.refIDs[path] = id
		}
		refTokens = append(refTokens, fmt.Sprintf("$%d", id))
	}

	prompt := anyToString(p["prompt"])
	line := prompt
	if len(refTokens) > 0 {
		line = strings.Join(refTokens, " ") + " " + prompt
	}
	existing := snapshotPNGs(r.istate.tmpDir)
	if err := r.interactiveSendLocked(line); err != nil {
		job.fail("interactive iris has exited; load it again")
		return
	}
	r.istate.nextRefID++ // the image this generation produces also consumes a $N slot
	command := append(append([]string{}, cmds...), line)
	job.set(func(j *Job) { j.Command = command; j.State = "running" })
	r.Emit("job", job.Summary())

	output, err := r.awaitGeneratedImage(job, existing, time.Hour)
	finished := nowSeconds()
	job.set(func(j *Job) { j.Finished = &finished })
	if err != nil {
		job.fail(err.Error())
		return
	}
	stem := safeStem(firstString(anyToString(p["label"]), "take"))
	name := fmt.Sprintf("%s-%s.png", stem, time.Now().Format("0102-150405"))
	dst := filepath.Join(outputs, name)
	if err := copyFile(output, dst); err != nil {
		job.fail(err.Error())
		return
	}
	job.set(func(j *Job) { j.State = "done"; j.Output = &name; j.OutputPath = dst })
	// Outside the lock: the sidecar is written from the job's Summary, which
	// takes that same lock.
	writeSidecar(dst, job)
}

// awaitGeneratedImage waits for the current interactive generation to
// finish. It races two independent signals because iris_cli.c's "Done -> "
// confirmation is printed via unflushed stdout buffering (unreliable over a
// pipe), while its per-step Kitty preview frames and stderr progress chars
// flush immediately (terminals.c calls fflush(stdout) explicitly, and
// stderr is unbuffered by the C standard) — so both the best-effort "Done"
// line and a filesystem poll of iris's own temp output directory are
// watched, whichever notices completion first wins.
func (r *Runner) awaitGeneratedImage(job *Job, existing map[string]int64, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	pollEvery := 400 * time.Millisecond
	nextPoll := time.Now().Add(pollEvery)
	stableSince := map[string]int64{}
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		wait := time.Until(nextPoll)
		if wait < 0 {
			wait = 0
		}
		select {
		case line := <-r.interactiveLines:
			if line == nil {
				return "", fmt.Errorf("interactive iris exited unexpectedly")
			}
			text := *line
			// Logged short, matched whole: truncateKittyLine cuts a preview
			// frame's base64 payload, and what the patterns below look for
			// could be on the part it cuts.
			logged := truncateKittyLine(text)
			// Interactive renders are reported to helmstudio the same way
			// one-shot ones are; this is that path's handleLine.
			var num, den int
			job.set(func(j *Job) {
				j.Log = append(j.Log, logged)
				if len(j.Log) > 400 {
					j.Log = j.Log[100:]
				}
				if m := stepProgRe.FindStringSubmatch(text); len(m) == 3 {
					j.Phase = "Denoising"
					num, den = intFrom(m[1], 0), intFrom(m[2], 0)
					j.Progress = []int{num, den}
				}
			})
			task := job.helm()
			task.Log(logged)
			task.Progress(num, den)
			if m := errorLnRe.FindStringSubmatch(text); len(m) == 2 {
				return "", fmt.Errorf("%s", m[1])
			}
			if m := doneLnRe.FindStringSubmatch(text); len(m) == 3 {
				return stringsTrimSpace(m[1]), nil
			}
			r.Emit("job", job.Summary())
		case <-time.After(minDuration(remaining, wait)):
			if time.Now().Before(nextPoll) {
				continue
			}
			nextPoll = time.Now().Add(pollEvery)
			if r.istate.tmpDir == "" {
				continue
			}
			entries, err := os.ReadDir(r.istate.tmpDir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || !strings.HasSuffix(name, ".png") {
					continue
				}
				path := filepath.Join(r.istate.tmpDir, name)
				info, err := e.Info()
				if err != nil {
					continue
				}
				if prevSize, seen := existing[path]; seen && prevSize == info.Size() {
					continue // untouched leftover from before this generation
				}
				size := info.Size()
				if prev, ok := stableSince[path]; ok && prev == size && size > 0 {
					return path, nil
				}
				stableSince[path] = size
			}
		}
	}
	return "", fmt.Errorf("timed out waiting for iris")
}

func snapshotPNGs(dir string) map[string]int64 {
	out := map[string]int64{}
	if dir == "" {
		return out
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".png") {
			continue
		}
		if info, err := e.Info(); err == nil {
			out[filepath.Join(dir, e.Name())] = info.Size()
		}
	}
	return out
}

// discoverIrisTmpDir locates the /tmp/iris-XXXXXX directory a freshly
// spawned interactive iris process creates for itself (via mkdtemp, very
// early in startup) so completed generations can be found even when the
// "Done -> " confirmation line never flushes through the pipe.
func discoverIrisTmpDir(after time.Time, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob("/tmp/iris-*")
		var candidates []string
		var mtimes []time.Time
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil || !info.IsDir() {
				continue
			}
			if info.ModTime().Before(after.Add(-2 * time.Second)) {
				continue
			}
			candidates = append(candidates, m)
			mtimes = append(mtimes, info.ModTime())
		}
		if len(candidates) > 0 {
			sort.Slice(candidates, func(i, j int) bool { return mtimes[i].After(mtimes[j]) })
			return candidates[0], nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("could not locate iris's temp output directory")
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Kitty graphics protocol preview parsing
// ---------------------------------------------------------------------------

// previewState tracks an in-progress Kitty-protocol frame across the lines
// it's split over. iris (via terminals.c, same encoding h3.c uses) writes
// raw pixel data as one or more "\033_G<attrs>;<base64>\033\\" chunks with
// m=1 on every chunk but the last (m=0).
type previewState struct {
	step   int
	format int // 24 (RGB) or 32 (RGBA), from the f= attribute
	width  int
	height int
	chunk  []byte
}

func newPreviewState() *previewState { return &previewState{step: -1} }

func tryParsePreviewLine(line string, st *previewState) (dataURL string, width, height int, done bool) {
	const gStart, gTerm = "\033_G", "\033\\"
	cursor := 0
	for {
		rel := strings.Index(line[cursor:], gStart)
		if rel < 0 {
			return
		}
		segStart := cursor + rel
		rest := line[segStart+len(gStart):]
		semi := strings.Index(rest, ";")
		if semi < 0 {
			return
		}
		relTerm := strings.Index(rest[semi+1:], gTerm)
		if relTerm < 0 {
			return
		}
		attrs := rest[:semi]
		data := strings.TrimRight(rest[semi+1:semi+1+relTerm], " \t\r\n")
		if f := findKV(attrs, "f="); f != "" {
			if n, err := strconv.Atoi(f); err == nil {
				st.format = n
			}
		}
		if s := findKV(attrs, "s="); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				st.width = n
			}
		}
		if v := findKV(attrs, "v="); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				st.height = n
			}
		}
		if data != "" {
			st.chunk = append(st.chunk, []byte(data)...)
		}
		cursor = segStart + len(gStart) + semi + 1 + relTerm + len(gTerm)
		more := findKV(attrs, "m=")
		if more == "" || more == "0" {
			if len(st.chunk) > 0 {
				if url, ok := encodeRawPNGDataURL(string(st.chunk), st.width, st.height, st.format); ok {
					dataURL = url
					done = true
				}
			}
			width, height = st.width, st.height
			st.chunk = nil
			st.width, st.height, st.format = 0, 0, 0
			return
		}
	}
}

// encodeRawPNGDataURL takes the raw Kitty-protocol payload (base64-encoded
// 24-bit RGB or 32-bit RGBA pixels — NOT a PNG file) and re-encodes it as an
// actual PNG so browsers can display it via an <img> data: URL.
func encodeRawPNGDataURL(rawBase64 string, width, height, format int) (string, bool) {
	if width <= 0 || height <= 0 {
		return "", false
	}
	bpp := 3
	if format == 32 {
		bpp = 4
	}
	raw, err := base64.StdEncoding.DecodeString(rawBase64)
	if err != nil {
		return "", false
	}
	if len(raw) < width*height*bpp {
		return "", false
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		row := y * width * bpp
		for x := 0; x < width; x++ {
			o := row + x*bpp
			a := uint8(255)
			if bpp == 4 {
				a = raw[o+3]
			}
			img.SetNRGBA(x, y, color.NRGBA{R: raw[o], G: raw[o+1], B: raw[o+2], A: a})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", false
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), true
}

// truncateKittyLine shortens a raw Kitty-protocol escape line (which can run
// to hundreds of KB of base64 for a single frame) before it goes into the
// terminal log/SSE feed — the full line is only needed by the preview parser.
func truncateKittyLine(line string) string {
	const max = 80
	if !strings.Contains(line, "\033_G") || len(line) <= max {
		return line
	}
	return line[:max] + "…"
}

func findKV(attrs, prefix string) string {
	idx := strings.Index(attrs, prefix)
	if idx < 0 {
		return ""
	}
	rest := attrs[idx+len(prefix):]
	end := strings.IndexAny(rest, ",; \t\n\r")
	if end <= 0 {
		return rest
	}
	return rest[:end]
}

func randomID() string {
	buf := make([]byte, 5)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
