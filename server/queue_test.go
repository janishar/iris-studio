package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	helm "github.com/janishar/helmstudio/packages/helm-runtime-sdk/go"
)

// What makes renders safe to stack: a queued render disturbs neither the one
// running nor the session on screen, and what it needs is still there when it
// gets to run. All of it runs on the stub iris and under t.TempDir().

// A render logs into its own session, whichever one the page is showing. It
// used to log into the active one, so a switch moved a render's log there.
func TestARenderLogsIntoItsOwnSessionWhicheverIsOnScreen(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	if _, _, err := cfg.ActivateSession("elsewhere"); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[string]string{"interactive": "iris> 256x256 a take", "oneshot": "Loading VAE"} {
		params := interactiveParams("a take")
		params["run_mode"] = mode
		job, err := runner.Submit(params)
		if err != nil {
			t.Fatal(err)
		}
		jobEnds(t, job)
		if got := terminalLog(t, cfg, "refs"); !strings.Contains(got, want) {
			t.Errorf("%s: the render's session logged %q", mode, got)
		}
		if got := terminalLog(t, cfg, "elsewhere"); got != "" {
			t.Errorf("%s: the session on screen logged %q", mode, got)
		}
	}
}

// Queueing used to empty the session's log, with the render before it still
// writing there.
func TestQueueingARenderLeavesTheRunningOnesLogAlone(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	running, err := runner.Submit(interactiveParams("hang"))
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		return strings.Contains(terminalLog(t, cfg, "refs"), "256x256 hang")
	})
	queued, err := runner.Submit(interactiveParams("next"))
	if err != nil {
		t.Fatal(err)
	}
	if got := terminalLog(t, cfg, "refs"); !strings.Contains(got, "256x256 hang") {
		t.Errorf("queueing a render left the log as %q", got)
	}
	runner.Cancel(queued.ID)
	runner.Cancel(running.ID)
	jobEnds(t, running)
}

func TestViewingASessionKeepsItsLog(t *testing.T) {
	cfg, err := NewConfig(Args{Iris: "iris", Model: "model", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.TerminalLog("a"), []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"a", "b", "a"} {
		if _, _, err := cfg.ActivateSession(session); err != nil {
			t.Fatal(err)
		}
	}
	if got := terminalLog(t, cfg, "a"); got != "kept\n" {
		t.Errorf("the log is %q", got)
	}
}

// A take's URL names its session, so it keeps meaning that take after a switch.
// With none named it is the active session's, as every URL used to be.
func TestAMediaURLMeansTheSessionItNames(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	srv := httptest.NewServer(NewApp(cfg, runner))
	defer srv.Close()
	for _, session := range []string{"alpha", "beta"} {
		inputs, outputs, err := cfg.SessionDirs(session)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{filepath.Join(inputs, "x.png"), filepath.Join(outputs, "x.png")} {
			if err := os.WriteFile(path, []byte(session), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Where a session called . or .. would reach, had it been let through.
	for _, dir := range []string{filepath.Dir(cfg.Sessions), cfg.Sessions} {
		for _, sub := range []string{"inputs", "outputs"} {
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, sub, "x.png"), []byte("outside"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, _, err := cfg.ActivateSession("beta"); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"/media/output/x.png", "/media/input/x.png", "/download/x.png"} {
		if got := string(get(t, srv.URL+url+"?session=alpha")); got != "alpha" {
			t.Errorf("%s of alpha is %q", url, got)
		}
		if got := string(get(t, srv.URL+url)); got != "beta" {
			t.Errorf("%s of the active session is %q", url, got)
		}
		for _, session := range []string{"ghost", "..", ".", "../beta", "..%2Fbeta"} {
			if code := status(t, srv.URL+url+"?session="+session); code != http.StatusNotFound {
				t.Errorf("%s of %q answered %d", url, session, code)
			}
		}
	}
	if DirExists(filepath.Join(cfg.Sessions, "ghost")) {
		t.Error("asking for a file made a session")
	}
}

// Past the queue's last slot a render is refused, and nothing is left of it.
// The REPL reads as busy for the length of this test, which stops the worker:
// what is queued stays queued.
func TestAFullQueueRefusesARender(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	runner.interactiveBusy(true, "Generating", nil)
	srv := httptest.NewServer(NewApp(cfg, runner))
	defer srv.Close()

	var refused error
	for i := 0; refused == nil; i++ {
		if i > cap(runner.queue)+1 {
			t.Fatal("the queue never filled")
		}
		_, refused = runner.Submit(interactiveParams("a take"))
	}
	waiting := len(runner.QueueState())

	code, answer := post(t, srv.URL, "/api/render", map[string]any{
		"session_name": "refs", "prompt": "one too many", "width": 256, "height": 256,
	})
	if code != http.StatusTooManyRequests || !strings.Contains(fmt.Sprint(answer["error"]), "queue is full") {
		t.Errorf("/api/render answered %d: %v", code, answer)
	}
	if got := len(runner.QueueState()); got != waiting {
		t.Errorf("the queue went from %d to %d", waiting, got)
	}
}

// A job holds a reference's name and reads the file when it runs. The stopped
// worker keeps this one queued.
func TestAReferenceAQueuedRenderUsesCannotBeDeleted(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	runner.interactiveBusy(true, "Generating", nil)
	srv := httptest.NewServer(NewApp(cfg, runner))
	defer srv.Close()
	addRef(t, cfg, "used.png", "used")
	addRef(t, cfg, "spare.png", "spare")
	if _, _, err := cfg.ActivateSession("refs"); err != nil {
		t.Fatal(err)
	}
	job, err := runner.Submit(interactiveParams("a take", "used.png"))
	if err != nil {
		t.Fatal(err)
	}

	remove := func(name string) (int, map[string]any) {
		return post(t, srv.URL, "/api/delete", map[string]any{"name": name, "kind": "image"})
	}
	code, answer := remove("used.png")
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(answer["error"]), "used.png") {
		t.Fatalf("/api/delete answered %d: %v", code, answer)
	}
	if !FileExists(filepath.Join(cfg.CurrentInputs(), "used.png")) {
		t.Fatal("the refused delete deleted it")
	}
	if code, answer := remove("spare.png"); code != http.StatusOK {
		t.Errorf("a reference nothing uses: %d: %v", code, answer)
	}

	// The same name in another session is another file.
	if _, _, err := cfg.ActivateSession("other"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.CurrentInputs(), "used.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, answer := remove("used.png"); code != http.StatusOK {
		t.Errorf("another session's file: %d: %v", code, answer)
	}

	if _, _, err := cfg.ActivateSession("refs"); err != nil {
		t.Fatal(err)
	}
	runner.Cancel(job.ID)
	if code, answer := remove("used.png"); code != http.StatusOK {
		t.Errorf("after the render was cancelled: %d: %v", code, answer)
	}
}

// A job holds its session by name too, and deleted under it the session comes
// back as a directory the session list skips, with the job's take inside.
func TestASessionWithARenderWaitingCannotBeDeleted(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	runner.interactiveBusy(true, "Generating", nil)
	srv := httptest.NewServer(NewApp(cfg, runner))
	defer srv.Close()
	addRef(t, cfg, "used.png", "used")
	job, err := runner.Submit(interactiveParams("a take", "used.png"))
	if err != nil {
		t.Fatal(err)
	}

	remove := func() (int, map[string]any) {
		return post(t, srv.URL, "/api/session/delete", map[string]any{"name": "refs"})
	}
	code, answer := remove()
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(answer["error"]), "refs") {
		t.Fatalf("/api/session/delete answered %d: %v", code, answer)
	}
	if !FileExists(filepath.Join(cfg.Sessions, "refs", "inputs", "used.png")) {
		t.Fatal("the refused delete deleted it")
	}

	runner.Cancel(job.ID)
	if code, answer := remove(); code != http.StatusOK {
		t.Errorf("after the render was cancelled: %d: %v", code, answer)
	}
}

// A saved form does not switch the session: a save landing after the page moved
// on would otherwise move it back.
func TestSavingASessionDoesNotActivateIt(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	srv := httptest.NewServer(NewApp(cfg, runner))
	defer srv.Close()
	active := cfg.CurrentSession()
	code, answer := post(t, srv.URL, "/api/session/save", map[string]any{"session_name": "another", "prompt": "a cat"})
	if code != http.StatusOK {
		t.Fatalf("%d: %v", code, answer)
	}
	if got := cfg.CurrentSession(); got != active {
		t.Errorf("the active session went from %q to %q", active, got)
	}
}

// A render typed into the terminal owns the next "Done -> ". A job sent in
// behind it would take the terminal's image for its own, so it waits.
func TestARenderCannotClaimAnImageTheTerminalAskedFor(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	if ok, msg := runner.LoadInteractive(map[string]any{"session_name": "typed"}); !ok {
		t.Fatalf("load interactive iris: %s", msg)
	}
	if ok, msg := runner.SendInteractive("a slow hold"); !ok {
		t.Fatalf("send: %s", msg)
	}
	waitUntil(t, func() bool { return runner.Busy()["busy"] == true })

	job, err := runner.Submit(interactiveParams("a take"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if state := job.state(); state != "queued" {
		t.Fatalf("the job went %s behind a busy REPL", state)
	}

	// A second line would run in front of the job, once the first one's Done
	// had let it in.
	if ok, _ := runner.SendInteractive("a second prompt"); ok {
		t.Error("a line went in behind a generation")
	}

	if err := os.WriteFile(filepath.Join(cfg.Workdir, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if state := jobEnds(t, job); state != "done" {
		t.Fatalf("state %q: %v", state, job.Summary())
	}
	if got, _ := os.ReadFile(job.outputPath()); string(got) != "a generated image" {
		t.Errorf("the job's take is %q", got)
	}

	// iris said no size for this one, because a reference decides it, so the
	// take's comes from the file. The fixture is 1x1.
	_, outputs, err := cfg.SessionDirs("typed")
	if err != nil {
		t.Fatal(err)
	}
	side := strings.TrimSuffix(waitForPNGs(t, outputs, 1, 20*time.Second)[0], ".png") + ".json"
	waitUntil(t, func() bool { return FileExists(side) })
	var meta struct {
		Params struct{ Width, Height float64 }
	}
	raw, err := os.ReadFile(side)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Params.Width != 1 || meta.Params.Height != 1 {
		t.Errorf("the terminal's take is %vx%v", meta.Params.Width, meta.Params.Height)
	}
}

// iris words the start of a generation in several ways, and a reference-sized
// one takes the longest to reach a step line. Each has to hold the queue back.
func TestEveryWayIrisStartsAGenerationReadsAsBusy(t *testing.T) {
	_, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	for _, line := range []string{
		"Generating 512x512...",
		"Generating 512x512 (img2img)...",
		"Generating 512x512 (multi-ref, 2 images)...",
		"Generating at reference size (img2img)...",
		"Generating at reference size (multi-ref, 2 images)...",
		"Generating 2 images at 256x256...",
	} {
		runner.interactiveActivity(line)
		if runner.Busy()["busy"] != true {
			t.Errorf("%q does not read as busy", line)
		}
		runner.interactiveBusy(false, "", nil)
	}
	runner.interactiveActivity("Loading VAE...")
	if runner.Busy()["busy"] == true {
		t.Error("loading reads as busy")
	}
}

// A job is cancelled, and that sticks, in the time between the worker taking it
// and it running: Cancel has said it worked, and this used to run it anyway.
func TestARenderCancelledAsItStartsStaysCancelled(t *testing.T) {
	_, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	for mode, start := range map[string]func(*Job){"oneshot": runner.run, "interactive": runner.runInteractive} {
		params := interactiveParams("a take")
		params["run_mode"] = mode
		job := newJob(params)
		job.set(func(j *Job) { j.State = "cancelled" })
		start(job)
		if state := job.state(); state != "cancelled" {
			t.Errorf("%s: state %q: %v", mode, state, job.Summary())
		}
	}
}

// A one-shot spends a while unloading the REPL with no process of its own to
// stop. Cancel used to answer false there and leave it running.
func TestARenderCanBeCancelledWhileTheReplUnloads(t *testing.T) {
	stopping := filepath.Join(t.TempDir(), "stopping")
	stub := filepath.Join(t.TempDir(), "iris")
	script := `#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -z "$out" ]; then
  while read -r _; do :; done
  touch "` + stopping + `"
  sleep 0.5
  exit 0
fi
cp "` + fixturePNG(t) + `" "$out"
sleep 30
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewConfig(Args{Iris: stub, Model: fakeModelDir(t), Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(cfg)
	defer runner.StopInteractive()
	if ok, msg := runner.LoadInteractive(map[string]any{"session_name": "s"}); !ok {
		t.Fatalf("load interactive iris: %s", msg)
	}
	job, err := runner.Submit(map[string]any{"session_name": "s", "prompt": "a", "run_mode": "oneshot"})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return FileExists(stopping) })
	if !runner.Cancel(job.ID) {
		t.Fatal("Cancel refused a render that was unloading the REPL")
	}
	if state := jobEnds(t, job); state != "cancelled" {
		t.Fatalf("state %q: %v", state, job.Summary())
	}
}

// The check that keeps two checkpoints from stacking is made as a one-shot
// starts, so a REPL loaded after that has to be refused.
func TestTheReplCannotBeLoadedDuringARender(t *testing.T) {
	_, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	runner.mu.Lock()
	runner.current = newJob(map[string]any{"session_name": "s"})
	runner.mu.Unlock()
	if ok, _ := runner.LoadInteractive(map[string]any{"session_name": "s"}); ok {
		t.Error("a REPL loaded alongside a render")
	}
	if runner.interactiveLoaded() {
		t.Error("the REPL is loaded")
	}
	runner.mu.Lock()
	runner.current = nil
	runner.mu.Unlock()
}

// The page has only this to go on when a one-shot unloads the REPL.
// Generate can start a REPL as well as Load can, and the page says what the next
// render will use from whether one is there.
func TestThePageIsToldWhenTheReplStarts(t *testing.T) {
	_, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	events := runner.Subscribe()
	defer runner.Unsubscribe(events)
	if ok, msg := runner.LoadInteractive(map[string]any{"session_name": "s"}); !ok {
		t.Fatalf("load interactive iris: %s", msg)
	}
	for len(events) > 0 {
		var e struct {
			Kind    string `json:"kind"`
			Payload struct {
				Loaded *bool `json:"loaded"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(<-events), &e) == nil && e.Kind == "interactive" && e.Payload.Loaded != nil {
			if !*e.Payload.Loaded {
				t.Error("told the REPL went")
			}
			return
		}
	}
	t.Error("never told")
}

func TestThePageIsToldWhenTheReplGoes(t *testing.T) {
	_, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	if ok, msg := runner.LoadInteractive(map[string]any{"session_name": "s"}); !ok {
		t.Fatalf("load interactive iris: %s", msg)
	}
	events := runner.Subscribe()
	defer runner.Unsubscribe(events)
	runner.StopInteractive()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case raw := <-events:
			var e struct {
				Kind    string `json:"kind"`
				Payload struct {
					Loaded *bool `json:"loaded"`
				} `json:"payload"`
			}
			if json.Unmarshal([]byte(raw), &e) == nil && e.Kind == "interactive" && e.Payload.Loaded != nil {
				if *e.Payload.Loaded {
					t.Error("told the REPL is loaded")
				}
				return
			}
		case <-timeout:
			t.Fatal("never told")
		}
	}
}

// A render being cancelled is still drawing, into the session it came from, and
// the page learns whose frames they are from this list.
func TestACancellingRenderStaysInTheQueue(t *testing.T) {
	_, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	runner.interactiveBusy(true, "Generating", nil)
	job, err := runner.Submit(interactiveParams("a take"))
	if err != nil {
		t.Fatal(err)
	}
	job.set(func(j *Job) { j.State = "cancelling" })
	if !runner.sessionInUse("refs") {
		t.Errorf("the queue is %v", runner.QueueState())
	}
}

// While a render takes the REPL's lines the reader reports nothing, so a busy it
// reported just before, for a generation that render ends up taking, would
// never clear and the queue behind it would wait for good.
func TestARenderTakingTheReplClearsWhatTheReaderLastSaid(t *testing.T) {
	_, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	runner.interactiveBusy(true, "Generating", nil)
	runner.setAwaitingImage(true)
	defer runner.setAwaitingImage(false)
	if runner.Busy()["busy"] == true {
		t.Error("still busy with no generation of the terminal's to wait for")
	}
}

// The stub's one-shot refuses to run while its REPL is up, as the memory peak
// does: the REPL has to be gone before iris starts, not merely by the end.
func TestAOneShotRenderRunsWithTheReplUnloaded(t *testing.T) {
	loaded := filepath.Join(t.TempDir(), "repl-loaded")
	stub := filepath.Join(t.TempDir(), "iris")
	script := `#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -z "$out" ]; then
  touch "` + loaded + `"
  while read -r _; do :; done
  rm "` + loaded + `"
  exit 0
fi
if [ -e "` + loaded + `" ]; then
  echo "Error: the REPL still holds its model" >&2
  exit 1
fi
cp "` + fixturePNG(t) + `" "$out"
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewConfig(Args{Iris: stub, Model: fakeModelDir(t), Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(cfg)
	defer runner.StopInteractive()
	if ok, msg := runner.LoadInteractive(map[string]any{"session_name": "s"}); !ok {
		t.Fatalf("load interactive iris: %s", msg)
	}
	waitUntil(t, func() bool { return FileExists(loaded) })

	job, err := runner.Submit(map[string]any{"session_name": "s", "prompt": "a", "run_mode": "oneshot"})
	if err != nil {
		t.Fatal(err)
	}
	if state := jobEnds(t, job); state != "done" {
		t.Fatalf("state %q: %v", state, job.Summary())
	}
	if runner.interactiveLoaded() {
		t.Error("the REPL is still loaded")
	}
}

// A cancel from the launcher arrives as a job event carrying the request, and
// stops the render on that helmstudio job and no other.
func TestACancelFromTheLauncherStopsTheRenderOnThatJob(t *testing.T) {
	_, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	job, err := runner.Submit(interactiveParams("hang"))
	if err != nil {
		t.Fatal(err)
	}
	for job.state() != "running" || !runner.awaitingImage() {
		time.Sleep(5 * time.Millisecond)
	}
	job.set(func(j *Job) { j.task = &Task{id: "this-render", flushed: make(chan struct{})} })

	events := []string{
		`{"job":{"id":"this-render","state":"running"}}`,
		`{"job":{"id":"another","state":"running","cancel_requested_at":"2026-10-31T10:00:00Z"}}`,
		`{"job":{"id":"this-render","state":"running","cancel_requested_at":"2026-10-31T10:00:00Z"}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i, data := range events {
			fmt.Fprintf(w, "id: boot-%d\nevent: job\ndata: %s\n\n", i, data)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	named := make(chan string, len(events))
	platform := &Platform{client: helm.NewRemote(srv.URL, "a-token")}
	go platform.WatchCancels(ctx, func(id string) {
		named <- id
		runner.cancelHelmJob(id)
	})
	if id := <-named; id != "another" {
		t.Errorf("first cancel named %q", id)
	}
	if id := <-named; id != "this-render" {
		t.Errorf("second cancel named %q", id)
	}
	if state := jobEnds(t, job); state != "cancelled" {
		t.Fatalf("state %q: %v", state, job.Summary())
	}
}

// helmstudio's parseBusy reads keys exactly, and a progress outside 0..1, or not
// a number, turns the whole answer into unknown. A job's progress is [n, total],
// so that shape cannot be what goes out.
func TestTheBusyProbeAnswersHelmstudiosContract(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	srv := httptest.NewServer(NewApp(cfg, runner))
	defer srv.Close()
	ask := func() (bool, bool, string, float64) {
		t.Helper()
		var answer map[string]any
		if err := json.Unmarshal(get(t, srv.URL+"/api/busy"), &answer); err != nil {
			t.Fatal(err)
		}
		if len(answer) != 4 {
			t.Errorf("keys: %v", answer)
		}
		busy, ok1 := answer["busy"].(bool)
		loaded, ok2 := answer["loaded"].(bool)
		message, ok3 := answer["message"].(string)
		progress, ok4 := answer["progress"].(float64)
		if !ok1 || !ok2 || !ok3 || !ok4 || progress < 0 || progress > 1 {
			t.Fatalf("not the contract: %#v", answer)
		}
		return busy, loaded, message, progress
	}

	if busy, loaded, _, progress := ask(); busy || loaded || progress != 0 {
		t.Errorf("idle: busy %v, loaded %v, progress %v", busy, loaded, progress)
	}

	// A render the terminal asked for has no job and is busy all the same.
	runner.interactiveBusy(true, "Denoising", []int{1, 4})
	if busy, _, message, progress := ask(); !busy || message != "Denoising" || progress != 0.25 {
		t.Errorf("terminal: busy %v, message %q, progress %v", busy, message, progress)
	}
	runner.interactiveBusy(false, "", nil)

	job := newJob(map[string]any{"session_name": "s"})
	job.set(func(j *Job) { j.Phase, j.Progress = "Denoising", []int{3, 4} })
	runner.mu.Lock()
	runner.current = job
	runner.mu.Unlock()
	if busy, loaded, _, progress := ask(); !busy || !loaded || progress != 0.75 {
		t.Errorf("render: busy %v, loaded %v, progress %v", busy, loaded, progress)
	}
	runner.mu.Lock()
	runner.current = nil
	runner.mu.Unlock()
}

func terminalLog(t *testing.T, cfg *Config, session string) string {
	t.Helper()
	data, err := os.ReadFile(cfg.TerminalLog(session))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("it never happened")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func status(t *testing.T, url string) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func post(t *testing.T, base, path string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	res, err := http.Post(base+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	answer := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&answer)
	return res.StatusCode, answer
}
