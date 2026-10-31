package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// lastError is the last "Error: " iris printed, which says why a failed
	// render failed in a way its exit code does not.
	lastError string
	// width and height are the take's own, once there is one: params have none
	// when a ratio or a reference decided the size.
	width, height int64

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

// start moves a queued job to running, and says false if it cannot. The worker
// has a job some time before it runs it, and a cancel in between found the job
// still queued and was told it had worked.
func (j *Job) start() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.State != "queued" {
		return false
	}
	now := nowSeconds()
	j.State, j.Started = "running", &now
	return true
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
	// Shared, not copied, until there is a size to add: Params is written once
	// by newJob and never again, and this runs once per line of a render's
	// output.
	params := j.Params
	if j.width > 0 {
		params = cloneMap(params)
		params["width"], params["height"] = j.width, j.height
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
		"params":   params,
		"command":  append([]string(nil), j.Command...),
		"log":      append([]string(nil), log...),
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
	// nextRefID is the $N the next image will get, from the ids iris has
	// reported. Written by the reader and read by a render, so it is the one
	// field here that goes through Runner.mu.
	nextRefID int
}

func newInteractiveState() *interactiveState {
	return &interactiveState{schedule: "default", powerAlpha: 2.0, zoom: 2, refIDs: map[string]int{}, nextRefID: 1}
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

	interactiveLock sync.Mutex
	// awaiting is true while a render is waiting for the REPL to finish an
	// image. It decides who gets the REPL's lines: the render, or the idle
	// path below that keeps what the terminal asked for.
	awaiting bool
	// replBusy is true while the REPL generates something the terminal asked
	// for, with what it last said about it. The worker waits on replIdle for
	// it to clear: that generation owns the next "Done -> ", and a render sent
	// in behind it would be handed the terminal's image as its own.
	replBusy     bool
	replPhase    string
	replProgress []int
	replIdle     *sync.Cond
	lastAsked    string
	// What the REPL said about the generation now running, for the take it is
	// about to produce: a take the terminal asked for has no parameters
	// behind it, so this is what there is to describe it by.
	lastSeed         *int64
	interactiveProc  *exec.Cmd
	interactiveIn    io.WriteCloser
	interactiveDone  chan struct{}
	interactiveLines chan *string
	istate           *interactiveState
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
	r.replIdle = sync.NewCond(&r.mu)
	go r.loop()
	// For as long as the studio runs; with no platform it returns at once.
	go cfg.Platform.WatchCancels(context.Background(), r.cancelHelmJob)
	return r
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
				// The session travels with the line: a render's is its own,
				// whichever one the page has moved to since. With none named
				// it is the terminal's, and the active session's.
				path := r.cfg.TerminalLog(anyToString(m["session"]))
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

// errQueueFull is what a render gets when every slot of the queue is taken.
var errQueueFull = errors.New("the queue is full; wait for a render to finish")

// Submit queues a render, or refuses it when the queue is full: this runs on
// an HTTP goroutine, which has no business waiting for a render to end.
func (r *Runner) Submit(params map[string]any) (*Job, error) {
	job := newJob(params)
	r.mu.Lock()
	select {
	case r.queue <- job.ID:
	default:
		r.mu.Unlock()
		return nil, errQueueFull
	}
	r.jobs[job.ID] = job
	r.order = append(r.order, job.ID)
	r.mu.Unlock()
	r.Emit("queue", r.QueueState())
	return job, nil
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
		job.set(func(j *Job) { j.State = "cancelling" })
		r.mu.Unlock()
		// An interactive render has no process of its own to stop, only the
		// REPL it is running in. It ends when the reader sees that go. With
		// neither, a one-shot has not started iris yet, and run stops it when
		// it has.
		if proc != nil {
			go stopProcess(proc)
		} else if anyToString(job.Params["run_mode"]) == "interactive" {
			go r.StopInteractive()
		}
		return true
	}
	r.mu.Unlock()
	return false
}

// cancelHelmJob is a cancel from the launcher, which knows a render by its
// helmstudio job. Only the running render has one: a queued render opens its
// job when it starts.
func (r *Runner) cancelHelmJob(id string) {
	r.mu.Lock()
	job := r.current
	r.mu.Unlock()
	if job != nil && job.helm().JobID() == id {
		r.Cancel(job.ID)
	}
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

func (r *Runner) QueueState() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []map[string]any{}
	for _, id := range r.order {
		job := r.jobs[id]
		if job == nil {
			continue
		}
		// Cancelling stays: iris is still at it until the job ends, and its
		// session and references are still in use.
		if state := job.state(); state == "queued" || state == "running" || state == "cancelling" {
			out = append(out, job.Summary())
		}
	}
	return out
}

// referenceInUse reports whether a queued or running render of this session
// names this reference. A job holds only the name: the file is read when the
// render gets to it, so deleting it now would fail the render then.
func (r *Runner) referenceInUse(session, name string) bool {
	for _, job := range r.QueueState() {
		params, _ := job["params"].(map[string]any)
		if anyToString(params["session_name"]) != session {
			continue
		}
		refs, _ := params["refs"].([]any)
		for _, raw := range refs {
			if ref, ok := raw.(map[string]any); ok && anyToString(ref["name"]) == name {
				return true
			}
		}
	}
	return false
}

// sessionInUse reports whether a queued or running render belongs to this
// session. Deleted under one, the session comes back as a directory with no
// setting.json, which the session list skips, and the take with it.
func (r *Runner) sessionInUse(session string) bool {
	for _, job := range r.QueueState() {
		params, _ := job["params"].(map[string]any)
		if anyToString(params["session_name"]) == session {
			return true
		}
	}
	return false
}

// Busy is the answer to helmstudio's busy probe, whose contract is exact: its
// parseBusy reads a progress that is anything but a number in 0..1 as an answer
// it cannot use. A render the terminal asked for has no job, so busy reads the
// REPL as well as the queue.
func (r *Runner) Busy() map[string]any {
	queue := r.QueueState()
	r.mu.Lock()
	job, busy, message, progress := r.current, r.replBusy, r.replPhase, r.replProgress
	r.mu.Unlock()
	if job != nil {
		summary := job.Summary()
		message, _ = summary["phase"].(string)
		progress, _ = summary["progress"].([]int)
	}
	busy = busy || job != nil || len(queue) > 0
	if busy && message == "" {
		message = "Rendering"
	}
	fraction := 0.0
	if len(progress) == 2 && progress[1] > 0 {
		fraction = min(float64(progress[0])/float64(progress[1]), 1)
	}
	return map[string]any{
		"busy":     busy,
		"loaded":   job != nil || r.interactiveLoaded(),
		"message":  message,
		"progress": fraction,
	}
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
		for r.replBusy {
			r.replIdle.Wait()
		}
		job := r.jobs[jobID]
		if job == nil || job.state() == "cancelled" {
			r.mu.Unlock()
			continue
		}
		r.current = job
		r.mu.Unlock()
		// Here and not when it was queued, so queueing a render cannot wipe the
		// log of the one running.
		_ = os.WriteFile(r.cfg.TerminalLog(anyToString(job.Params["session_name"])), []byte{}, 0o644)
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
				// Read together, so the state the job ended in and the reason
				// it gives cannot come from two different moments.
				state, failure := job.outcome()
				if state == "done" {
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
	if plan := planSize(cfg, p); plan.Sent {
		args = append(args, "-W", anyToString(plan.Width), "-H", anyToString(plan.Height))
	}
	if steps := intFrom(p["steps"], 0); steps > 0 {
		args = append(args, "-s", anyToString(steps))
	}
	// iris picks a random seed once to print and again to render, so the only
	// seed it reports truthfully is one it was given.
	seed := int64From(p["seed"], -1)
	if seed < 0 {
		seed = time.Now().UnixMilli()
	}
	args = append(args, "-S", anyToString(seed))
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
	if !job.start() {
		return
	}
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
	job.set(func(j *Job) { j.Command = append([]string{}, args...) })
	r.Emit("job", job.Summary())
	r.Emit("queue", r.QueueState())
	// A loaded REPL holds its transformer, and iris loads another for this:
	// the two would stack past the peak the manifest declares.
	if r.interactiveLoaded() {
		r.Emit("terminal", map[string]any{"line": "[studio] unloading interactive iris for a one-shot render",
			"running": true, "session": p["session_name"]})
		r.StopInteractive()
	}
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
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		job.fail(err.Error())
		return
	}
	_ = writer.Close()
	// Only now: Cancel stops what it finds here, and Start is what makes that
	// possible. One that came earlier found nothing and left the job saying so.
	r.mu.Lock()
	r.proc = cmd
	r.mu.Unlock()
	if job.state() == "cancelling" {
		go stopProcess(cmd)
	}
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
	job.set(func(j *Job) {
		message := j.lastError
		if message == "" {
			message = fmt.Sprintf("iris exited with code %d", code)
		}
		j.State = "failed"
		j.Error = &message
	})
}

var (
	seedRe     = regexp.MustCompile(`^Seed:\s*(-?\d+)\s*$`)
	stepMarkRe = regexp.MustCompile(`^\[Step (\d+)\]\s*$`)
	// An error can follow a preview frame on the same line: stdout and stderr
	// share the pipe, and a frame is what iris last wrote to it.
	errorLnRe = regexp.MustCompile(`(?:^|\x1b\\)Error: (.*)$`)
	doneLnRe  = regexp.MustCompile(`^Done -> (.+?) \(ref \$(\d+)\)`)
	// Both "Done -> path (ref $3) [1.00s]" and "Loaded: path (64x64, ref $3)".
	loadedLnRe = regexp.MustCompile(`ref \$(\d+)\)`)
	// iris_cli.c starts every generation with "Generating ": a size, "at
	// reference size" when a reference decides it, or "%d images at %dx%d" for
	// cmd_explore, which ends on the second pattern here.
	generatingRe  = regexp.MustCompile(`^Generating `)
	exploreDoneRe = regexp.MustCompile(`^Done\. Use`)
	// iris_cli.c, cmd_size, which is how a bare "!size" is answered.
	sizeLnRe = regexp.MustCompile(`^Size: \d+x\d+`)
)

// Denoising progress, in the two shapes iris prints it. One binary prints
// both: which one depends on the flags, and iris.c/main.c decides between
// them on whether a prompt and an output path were given.
//
// stepBarRe is the one-shot CLI's (iris.c/main.c, cli_step_callback):
//
//	Step 2/4 ddddddddssssF
//
// It is written without a trailing newline, so the line only arrives when
// the next step begins with one — progress therefore lags a step, which is
// iris's to change and not this studio's. It is anchored because a prompt
// echoed back could otherwise say "Step 2/4", and it ends at a word
// boundary rather than the space iris writes, because pump trims that.
//
// stepProgRe is the REPL's (iris.c/iris_cli.c, cli_step_progress):
//
//	[2/4]:ddsssf
//
// Deliberately matched by neither: the timing breakdown printed after a
// render ("  Step 1: 1759.6 ms", iris.c/iris_sample.c) and the per-image
// counter !explore prints ("  [1/4] Seed: 4242", iris.c/iris_cli.c). Both
// would wind the bar backwards over a render that has already finished.
var (
	stepBarRe  = regexp.MustCompile(`^\s*Step (\d+)/(\d+)\b`)
	stepProgRe = regexp.MustCompile(`\[(\d+)/(\d+)\]:`)
)

// stepProgress is how far into denoising a line says the render is, and
// whether it said at all. Both shapes are read on both paths: one binary
// prints them, and which front-end is running is a detail of the flags.
func stepProgress(line string) (int, int, bool) {
	for _, re := range []*regexp.Regexp{stepBarRe, stepProgRe} {
		if m := re.FindStringSubmatch(line); len(m) == 3 {
			num, den := intFrom(m[1], 0), intFrom(m[2], 0)
			if den > 0 {
				return num, den, true
			}
		}
	}
	return 0, 0, false
}

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
		if m := errorLnRe.FindStringSubmatch(line); len(m) == 2 {
			j.lastError = m[1]
		}
		if n, total, ok := stepProgress(line); ok {
			j.Phase = "Denoising"
			num, den = n, total
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
	r.Emit("terminal", map[string]any{"line": text, "running": true, "session": job.Params["session_name"]})
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
	// A one-shot render holds a transformer of its own, and it has already
	// looked for a REPL to unload. Checked under interactiveLock, so one that
	// starts after this finds the lock taken and waits to unload this REPL.
	r.mu.Lock()
	busy := r.current != nil
	r.mu.Unlock()
	if busy {
		return false, "interactive iris is busy generating"
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
	// replBusy as well: a line sent during a generation runs when that one
	// ends, in front of whichever render the worker lets in on its Done.
	r.mu.Lock()
	busy := r.current != nil || r.replBusy
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
	// What the image this produces will be called by, since nothing else
	// describes a take the terminal asked for.
	r.mu.Lock()
	r.lastAsked = text
	r.mu.Unlock()
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
	go r.readInteractive(reader, cmd, r.interactiveDone, r.istate)
	return nil
}

func (r *Runner) readInteractive(reader *os.File, cmd *exec.Cmd, done chan struct{}, st *interactiveState) {
	defer reader.Close()
	br := bufio.NewReader(reader)
	var buf []byte
	preview := newPreviewState()
	afterDone := false
	session := ""
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
				// Asked before the line is handed on, and not for the frame iris
				// draws after Done: both are a render's, and it can be over by
				// the time they are logged.
				if !afterDone {
					session = r.jobSession()
				}
				// Before the line is handed on: a render that has seen Done
				// must find the id it carried already counted.
				if m := loadedLnRe.FindStringSubmatch(line); len(m) == 2 {
					r.mu.Lock()
					st.nextRefID = max(st.nextRefID, intFrom(m[1], 0)+1)
					r.mu.Unlock()
				}
				// Only a waiting render drains this channel. Sending when
				// none is would fill it — a generation writes hundreds of
				// lines — and then this goroutine blocks for good and the
				// terminal goes quiet.
				if r.awaitingImage() {
					r.interactiveLines <- &text
				} else if m := doneLnRe.FindStringSubmatch(line); len(m) == 3 {
					r.interactiveBusy(false, "", nil)
					go r.keepInteractiveImage(stringsTrimSpace(m[1]))
				} else if errorLnRe.MatchString(line) {
					r.interactiveBusy(false, "", nil)
				} else {
					r.interactiveActivity(line)
				}
				if m := stepMarkRe.FindStringSubmatch(line); len(m) == 2 {
					preview.step = intFrom(m[1], 0)
				}
				// iris draws the finished image right after saying Done. That
				// frame is the take, which reaches the viewer as a take; shown
				// as a preview it would unmark the take in the rail.
				finalFrame := afterDone
				afterDone = doneLnRe.MatchString(line)
				if afterDone {
					preview.step = -1
				}
				if url, w, h, done := tryParsePreviewLine(line, preview); url != "" && !finalFrame {
					r.Emit("preview", map[string]any{"url": url, "width": w, "height": h, "step": preview.step})
					_ = done
				}
				r.Emit("terminal", map[string]any{"line": truncateKittyLine(line), "running": true, "session": session})
			}
			continue
		}
		buf = append(buf, b)
	}
	_, _ = cmd.Process.Wait()
	// The render waiting on this reads nil as "iris exited". With none
	// waiting there is nobody to tell, and a nil left in the channel would
	// fail the next render as soon as it started. Asked before done closes,
	// which is what lets one start.
	waiting := r.awaitingImage()
	// Before anything below wakes the next render: it has to find this REPL
	// gone, not write to it.
	close(done)
	r.interactiveBusy(false, "", nil)
	r.Emit("interactive", map[string]any{"running": false, "loaded": false})
	if waiting {
		r.interactiveLines <- nil
	}
}

// interactiveActivity says a line means the REPL is working, so the page can
// show it. A render started by Generate reports its own progress through its
// job; one the terminal asked for has no job, and without this the page sits
// there looking idle for the minute it takes.
//
// What counts as work is what iris prints while doing it: the step counter,
// and the line it writes when it starts. Nothing guesses from the command —
// `!explore` generates and `!help` does not, and neither says so up front.
func (r *Runner) interactiveActivity(line string) {
	// The REPL says this before it says Done, and the prompt aside it is what
	// describes the take: iris_cli.c prints "Seed: %lld".
	if m := seedRe.FindStringSubmatch(line); len(m) == 2 {
		if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			r.mu.Lock()
			r.lastSeed = &v
			r.mu.Unlock()
		}
		return
	}
	if generatingRe.MatchString(line) {
		r.interactiveBusy(true, "Generating", nil)
		return
	}
	if exploreDoneRe.MatchString(line) {
		r.interactiveBusy(false, "", nil)
		return
	}
	if n, total, ok := stepProgress(line); ok {
		r.interactiveBusy(true, "Denoising", []int{n, total})
	}
}

// interactiveBusy tells the page whether the REPL is working, and how far in.
func (r *Runner) interactiveBusy(running bool, phase string, progress []int) {
	r.mu.Lock()
	r.replBusy, r.replPhase, r.replProgress = running, phase, progress
	r.replIdle.Broadcast()
	r.mu.Unlock()
	r.Emit("interactive", map[string]any{"running": running, "phase": phase, "progress": progress})
}

// awaitingImage reports whether a render is waiting for the REPL's next image.
func (r *Runner) awaitingImage() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.awaiting
}

// setAwaitingImage also clears replBusy, which only the reader's idle path can
// clear and which does not run while a render takes the lines. Left set, the
// queue behind this render would wait for good.
func (r *Runner) setAwaitingImage(v bool) {
	r.mu.Lock()
	r.awaiting = v
	r.replBusy, r.replPhase, r.replProgress = false, "", nil
	r.mu.Unlock()
}

// jobSession is the session of the render running now, or "" with none.
func (r *Runner) jobSession() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		return ""
	}
	return anyToString(r.current.Params["session_name"])
}

// interactiveLoaded reports whether a REPL holds a model. It only tries the
// lock: StopInteractive keeps it for seconds and helmstudio's probe waits for
// two. Whoever has it has a REPL or is waiting for one to go, so the answer
// then is yes.
func (r *Runner) interactiveLoaded() bool {
	if !r.interactiveLock.TryLock() {
		return true
	}
	defer r.interactiveLock.Unlock()
	r.normalizeInteractiveStateLocked()
	return r.interactiveProc != nil
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
	r.Emit("terminal", map[string]any{"line": "iris> " + flat, "running": true, "session": r.jobSession()})
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
	if !job.start() {
		return
	}
	p := job.Params
	outputs, err := r.startInteractive(job)
	var output string
	if err == nil {
		// Without interactiveLock: stopping the REPL needs it, and this is
		// what a render stuck in iris has to be stopped out of.
		output, err = r.awaitGeneratedImage(job, time.Hour)
	}
	r.setAwaitingImage(false)
	finished := nowSeconds()
	job.set(func(j *Job) { j.Finished = &finished })
	if job.state() == "cancelling" {
		job.set(func(j *Job) { j.State = "cancelled" })
		return
	}
	if err != nil {
		r.forgetRefIDs()
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

// forgetRefIDs drops every id a failed render left behind: a !load that failed
// took none, so it and any after it were guessed wrong. The next render that
// wants them loads them again.
func (r *Runner) forgetRefIDs() {
	r.interactiveLock.Lock()
	defer r.interactiveLock.Unlock()
	if r.istate != nil {
		r.istate.refIDs = map[string]int{}
	}
}

// startInteractive sends a render to the REPL and returns the directory its
// take belongs in. It leaves awaiting set whether or not it succeeds, for
// runInteractive to clear.
func (r *Runner) startInteractive(job *Job) (string, error) {
	p := job.Params
	refs, _ := p["refs"].([]any)
	r.interactiveLock.Lock()
	defer r.interactiveLock.Unlock()
	r.normalizeInteractiveStateLocked()
	if !isModelDir(r.cfg.Model) {
		return "", fmt.Errorf("configured model directory not found: %s", r.cfg.Model)
	}
	if err := r.ensureInteractiveLocked(); err != nil {
		return "", err
	}
	inputs, outputs, err := r.cfg.SessionDirs(anyToString(p["session_name"]))
	if err != nil {
		return "", err
	}

	plan := planSize(r.cfg, p)
	var cmds []string
	if plan.Sent {
		cmds = append(cmds, fmt.Sprintf("!size %dx%d", plan.Width, plan.Height))
	}
	cmds = append(cmds, fmt.Sprintf("!steps %d", intFrom(p["steps"], 0)))
	cmds = append(cmds, fmt.Sprintf("!seed %d", int64From(p["seed"], -1)))
	cmds = append(cmds, fmt.Sprintf("!guidance %.3f", floatFrom(p["guidance"], 0)))
	cmds = append(cmds, r.istate.syncSchedule(anyToString(firstNonEmpty(p["schedule"], "default")), floatFrom(p["power_alpha"], 2.0))...)
	cmds = append(cmds, r.istate.syncShowSteps(boolFrom(p["show_steps"]))...)
	cmds = append(cmds, r.istate.syncZoom(intFrom(p["zoom"], 2))...)
	for _, cmd := range cmds {
		if err := r.interactiveSendLocked(cmd); err != nil {
			return "", fmt.Errorf("interactive iris has exited; load it again")
		}
	}

	// From here an error is this render's, including one from a !load, so the
	// reader has to be sending lines here before the first of them is written.
	// What an earlier render left unread is not this one's.
	r.setAwaitingImage(true)
	for len(r.interactiveLines) > 0 {
		<-r.interactiveLines
	}

	// Resolve each reference to a $N token. iris_cli.c gives every image it
	// loads or generates the next id, from 1, and only one that worked. A
	// reference is loaded here without waiting for its "Loaded: " line, so
	// the ids are worked out from what iris last reported.
	r.mu.Lock()
	nextID := r.istate.nextRefID
	r.mu.Unlock()
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
				return "", fmt.Errorf("interactive iris has exited; load it again")
			}
			id = nextID
			nextID++
			r.istate.refIDs[path] = id
		}
		refTokens = append(refTokens, fmt.Sprintf("$%d", id))
	}

	// A size is on the line, because !load sets iris's size to the reference's,
	// and first, because iris_cli.c reads the rest of a line that starts with
	// one as text: a prompt can open with ! or $, or end in something like
	// 1024x1024, and stay a prompt. A line with none leaves the size to the
	// reference, and validate refuses the prompt that would then be read as
	// another reference or as a size.
	tokens := refTokens
	if plan.Sent {
		tokens = append(tokens, fmt.Sprintf("%dx%d", plan.Width, plan.Height))
	}
	line := strings.Join(append(tokens, anyToString(p["prompt"])), " ")
	if err := r.interactiveSendLocked(line); err != nil {
		return "", fmt.Errorf("interactive iris has exited; load it again")
	}
	command := append(append([]string{}, cmds...), line)
	job.set(func(j *Job) { j.Command = command })
	r.Emit("job", job.Summary())
	return outputs, nil
}

// keepInteractiveImage keeps an image the terminal asked for.
//
// The REPL writes every image into a temp directory of its own and announces
// it with a "Done -> " line; a render started by Generate is waiting for
// exactly that line and copies the file out of there. A line typed into the
// terminal has no render waiting, so without this the image stays in /tmp and
// goes when /tmp does. `!explore` is not this: iris writes none of its images
// to disk and says no "Done -> ", so they stay previews.
//
// It becomes a take like any other: the same outputs/ directory, the same
// sidecar and the same gallery item. Where it came from is recorded rather than
// hidden, because a take with no parameters behind it should say why.
func (r *Runner) keepInteractiveImage(src string) {
	if src == "" || !FileExists(src) {
		return
	}
	session := r.cfg.CurrentSession()
	_, outputs, err := r.cfg.SessionDirs(session)
	if err != nil {
		return
	}
	r.mu.Lock()
	asked, seed := r.lastAsked, r.lastSeed
	r.lastSeed = nil // this generation's, not the next one's
	r.mu.Unlock()

	stamp := time.Now().Format("0102-150405")
	name := fmt.Sprintf("take-%s.png", stamp)
	dst := filepath.Join(outputs, name)
	for i := 1; FileExists(dst); i++ {
		name = fmt.Sprintf("take-%s-%d.png", stamp, i)
		dst = filepath.Join(outputs, name)
	}
	if err := copyFile(src, dst); err != nil {
		r.Emit("terminal", map[string]any{"line": "!! could not keep " + src + ": " + err.Error(), "running": true})
		return
	}

	// A Job carries a take's description everywhere else here — the sidecar
	// and the gallery item are both written from one. This one was never
	// queued and never ran; it is the same description, for a take that
	// arrived the other way.
	now := nowSeconds()
	params := map[string]any{"session_name": session, "label": "terminal", "prompt": asked, "source": "terminal"}
	job := newJob(params)
	job.set(func(j *Job) {
		j.State = "done"
		j.Output = &name
		j.OutputPath = dst
		j.Started = &now
		j.Finished = &now
		j.Seed = seed
		j.Command = []string{asked}
	})
	writeSidecar(dst, job)
	r.cfg.Platform.RecordTake(dst, "terminal", session, job.Summary())
	r.Emit("outputs", listOutputs(r.cfg))
	r.Emit("terminal", map[string]any{"line": "[studio] kept as " + name, "running": true})
	// Naming the take here is what puts it in the viewer: a render started by
	// Generate is shown from its finished job, and this one has none.
	r.Emit("interactive", map[string]any{"running": false, "output": name})
}

// awaitGeneratedImage waits for the current interactive generation to finish,
// which iris announces with a "Done -> " line. Nothing else is taken for that:
// iris's temp directory also holds a copy of every !load.
func (r *Runner) awaitGeneratedImage(job *Job, timeout time.Duration) (string, error) {
	deadline := time.After(timeout)
	for {
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
				if m := seedRe.FindStringSubmatch(text); len(m) == 2 {
					if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
						j.Seed = &v
					}
				}
				if n, total, ok := stepProgress(text); ok {
					j.Phase = "Denoising"
					num, den = n, total
					j.Progress = []int{num, den}
				}
			})
			task := job.helm()
			task.Log(logged)
			task.Progress(num, den)
			if m := errorLnRe.FindStringSubmatch(text); len(m) == 2 {
				r.settleInteractive(deadline)
				return "", fmt.Errorf("%s", m[1])
			}
			if m := doneLnRe.FindStringSubmatch(text); len(m) == 3 {
				return stringsTrimSpace(m[1]), nil
			}
			r.Emit("job", job.Summary())
		case <-deadline:
			return "", fmt.Errorf("timed out waiting for iris")
		}
	}
}

// settleInteractive reads out what a failed render sent iris and has yet to
// hear about: a failed !load has the rest of the render's lines behind it, and
// what iris says to those is that render's, not the next one's. iris answers in
// order, so the answer to one more line says it has got through them all. A bare
// !size is that line: it changes nothing and generation never says the same.
func (r *Runner) settleInteractive(deadline <-chan time.Time) {
	r.interactiveLock.Lock()
	err := r.interactiveSendLocked("!size")
	r.interactiveLock.Unlock()
	if err != nil {
		return
	}
	for {
		select {
		case line := <-r.interactiveLines:
			if line == nil || sizeLnRe.MatchString(*line) {
				return
			}
		case <-deadline:
			return
		}
	}
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
// Whatever came after the frame on the same line is kept: it is text iris
// wrote, and an error can land there.
func truncateKittyLine(line string) string {
	const max = 80
	if !strings.Contains(line, "\033_G") || len(line) <= max {
		return line
	}
	short := line[:max] + "…"
	if i := strings.LastIndex(line, "\033\\"); i >= 0 {
		short += line[i+len("\033\\"):]
	}
	return short
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
