package server

import (
	"context"
	"errors"
	"image"
	_ "image/png" // the only format iris writes, and the only one this measures
	"log"
	"os"
	"sync"
	"time"

	helm "github.com/janishar/helmstudio/packages/helm-runtime-sdk/go"
)

// iris studio under helmstudio.
//
// A take is iris studio's own PNG under the session's outputs/ and stays that
// way; this records it with helmstudio as well, so what this studio made is in
// the gallery every studio shares. The file is adopted by hardlink, not
// copied — the same bytes, counted once — which is why sessions live in the
// directory helmstudio gives the studio and not in a checkout.
//
// There is always one: main resolves the platform before anything else and
// refuses to start the studio without it. The nil receivers below are written
// anyway — a platform that refuses one call must not take a render down with
// it, and a test may build a Config with no platform at all. Nothing in the
// render path depends on any of this succeeding.

// Platform is helmstudio: the daemon that launched this studio, or the one
// behind `helm dev`. It is nil only in tests.
type Platform struct {
	client *helm.Client
}

// NewPlatform returns helmstudio if this process is running under it, and nil
// if it is not. The reason is logged here; what a missing platform means is
// main's to decide, and main will not start without one.
func NewPlatform() *Platform {
	client, err := helm.FromEnv()
	if err != nil {
		if !errors.Is(err, helm.ErrNoProvider) {
			// HELM_API set but unusable: said here because the SDK knows
			// which part of it was wrong, and main only knows there is none.
			log.Printf("helmstudio: not recording takes: %v", err)
		}
		return nil
	}
	return &Platform{client: client}
}

// Available reports whether takes are being recorded with helmstudio.
func (p *Platform) Available() bool { return p != nil && p.client != nil }

// HelmAPI is the platform this process was launched with, or "" when it was
// launched with none. main reads it to tell the two ways there can be no
// platform apart: nothing launched this under helmstudio, or something did and
// the SDK could not use what it was given.
func HelmAPI() string { return os.Getenv(helm.EnvAPI) }

// RecordTake adopts a finished take and records it in the gallery, with the
// parameters it was made from. One call per take, which is what the gallery's
// contract asks for.
//
// It never fails a render. The take is already on disk and that is the source
// of truth; this is a record of it, and a platform that refuses is a line in
// the log rather than a lost generation.
//
// The session is deliberately not sent as session_id. helmstudio validates
// that against its own live sessions for this studio, and iris studio's
// sessions are still its own directories — so the name travels in params
// until sessions are helmstudio sessions too.
func (p *Platform) RecordTake(path, title, session string, params map[string]any) {
	if !p.Available() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	asset, err := p.client.Assets.Adopt(ctx, adoptRequestFor(path))
	if err != nil {
		log.Printf("helmstudio: could not adopt %s: %v", path, err)
		return
	}
	create := helm.ItemCreate{Kind: helm.AssetKindImage, AssetID: asset.ID, Params: itemParams(session, params)}
	if title != "" {
		create.Title = &title
	}
	if _, err := p.client.Gallery.Add(ctx, create); err != nil {
		log.Printf("helmstudio: adopted %s as %s but could not record it: %v", path, asset.ID, err)
	}
}

// adoptRequestFor is what helmstudio is told about a take: where it is, that
// it is an image, and how big it turned out.
//
// The size is read from the file rather than taken from the render's
// parameters, because the parameters are what was asked for and the header is
// what was made. A file that will not decode is adopted without dimensions
// rather than with guessed ones, because a guess here is a claim.
func adoptRequestFor(path string) helm.AdoptRequest {
	req := helm.AdoptRequest{Path: path, Kind: helm.AssetKindImage}
	f, err := os.Open(path)
	if err != nil {
		return req
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return req
	}
	w, h := int64(cfg.Width), int64(cfg.Height)
	req.Width, req.Height = &w, &h
	return req
}

// itemParams is the take's sidecar, plus the iris session it came from.
//
// It copies: params is the map the runner also writes to disk as the sidecar,
// and the caller still owns it. The session travels as a parameter because
// helmstudio validates session_id against its own live sessions and iris
// studio's are still its own directories.
func itemParams(session string, params map[string]any) map[string]any {
	item := make(map[string]any, len(params)+1)
	for k, v := range params {
		item[k] = v
	}
	if session != "" {
		item["iris_session"] = session
	}
	return item
}

// ---------------------------------------------------------------- task jobs

// A render, mirrored to helmstudio as a task job.
//
// iris studio's own runner is unchanged and stays in charge: it queues, runs,
// reports and cancels exactly as it did. This reports the same render to
// helmstudio in parallel, so the launcher can show what this studio is doing
// and helm-terminal has a log to stream. Nothing here can fail a render — a
// platform that refuses gets a line in the log and the render carries on.
//
// A nil *Task is a render helmstudio would not open a job for, and every
// method is a no-op on it, so the runner never asks whether there is one.
type Task struct {
	client *helm.Client
	id     string

	mu       sync.Mutex
	pending  []string
	progress []int // num, den; the latest, not every one
	closed   bool
	flushed  chan struct{}
	done     chan struct{}
}

// logFlush is how often buffered lines are sent. A render writes hundreds of
// them and one request per line would be a request per step; this trades a
// little latency for a request every half second.
const logFlush = 500 * time.Millisecond

// maxPendingLines bounds what a flush can owe, so a studio that floods its
// output cannot grow this without limit. The oldest go: helm-terminal is for
// watching a render, and the studio's own log keeps everything.
const maxPendingLines = 2000

// StartTask reports a render to helmstudio and returns the job to report it
// on, or nil when there is no platform or it refused.
func (p *Platform) StartTask() *Task {
	if !p.Available() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	state := string(helm.JobStateRunning)
	job, err := p.client.Jobs.Create(ctx, helm.TaskCreate{State: &state})
	if err != nil {
		log.Printf("helmstudio: not reporting this render: %v", err)
		return nil
	}
	t := &Task{client: p.client, id: job.ID, flushed: make(chan struct{}), done: make(chan struct{})}
	go t.pump()
	return t
}

// JobID is the helmstudio job this render is reported on, or "" when there is
// none. A nil Task answers "", so a caller never asks whether there is one.
func (t *Task) JobID() string {
	if t == nil {
		return ""
	}
	return t.id
}

// Log buffers one line for helmstudio. It never blocks the render and never
// writes from the caller's goroutine.
func (t *Task) Log(line string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.pending = append(t.pending, line)
	if over := len(t.pending) - maxPendingLines; over > 0 {
		t.pending = t.pending[over:]
	}
}

// pump is the only goroutine that talks to helmstudio about this job, so no
// call the render makes can wait on the network. It ends after one last flush,
// and closing done is what tells Finish the log is complete.
func (t *Task) pump() {
	defer close(t.done)
	tick := time.NewTicker(logFlush)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			t.flush()
		case <-t.flushed:
			t.flush()
			return
		}
	}
}

// flush sends whatever has accumulated: the lines, and the latest progress.
func (t *Task) flush() {
	t.mu.Lock()
	lines, progress := t.pending, t.progress
	t.pending, t.progress = nil, nil
	t.mu.Unlock()

	if len(lines) > 0 && t.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := t.client.Jobs.AppendLog(ctx, t.id, helm.LogAppend{Lines: lines}); err != nil {
			log.Printf("helmstudio: %d log lines not delivered: %v", len(lines), err)
		}
		cancel()
	}
	if len(progress) == 2 {
		t.patch(map[string]any{"progress_num": progress[0], "progress_den": progress[1]})
	}
}

// Progress records how far along the render is. It does not send: the render
// goroutine calls this, and nothing on the render path may wait on the
// network. The pump sends the latest value on its next tick, so a render that
// reports a hundred times between ticks costs one request, not a hundred.
func (t *Task) Progress(num, den int) {
	if t == nil || den <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.progress = []int{num, den}
}

// Finish closes the job in the state iris studio's own job ended in. Anything
// that is not done or cancelled is a failure, because a job left running is
// one the launcher would wait on forever.
func (t *Task) Finish(state, message string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	already := t.closed
	t.closed = true
	t.mu.Unlock()
	if already {
		return
	}
	// The last lines go before the job is closed, not beside it: a log append
	// to a job helmstudio already considers finished is a 409, and the end of
	// a render is exactly the part someone reads.
	close(t.flushed)
	if t.done != nil {
		<-t.done // a Task with no pump has nothing to wait for
	}

	body := map[string]any{"state": taskStateFor(state)}
	if body["state"] == string(helm.JobStateFailed) && message != "" {
		body["last_error"] = map[string]any{"code": "render_failed", "message": truncate(message, 4000)}
	}
	t.patch(body)
}

// taskStateFor maps iris studio's own job states onto the four a task job may
// be set to. "done" is what iris studio calls a finished render.
func taskStateFor(state string) string {
	switch state {
	case "done":
		return string(helm.JobStateSucceeded)
	case "cancelled":
		return string(helm.JobStateCancelled)
	default:
		return string(helm.JobStateFailed)
	}
}

func (t *Task) patch(body map[string]any) {
	if t == nil || t.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := t.client.Jobs.Update(ctx, t.id, body); err != nil {
		log.Printf("helmstudio: job %s not updated: %v", t.id, err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
