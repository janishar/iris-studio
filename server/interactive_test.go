package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A prompt typed into the terminal is a real generation: interactive iris
// writes it into a temp directory of its own and says "Done -> ". Nothing was
// waiting for that line, so the image used to stay there and go when /tmp did.
// It is kept as a take now, like one Generate asked for.
func TestAnImageTheTerminalAskedForIsKept(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()

	if ok, msg := runner.LoadInteractive(map[string]any{"session_name": "typed"}); !ok {
		t.Fatalf("load interactive iris: %s", msg)
	}
	// What the page would receive: the event naming the take to show.
	kept := make(chan string, 1)
	go func() {
		events := runner.Subscribe()
		defer runner.Unsubscribe(events)
		for raw := range events {
			var e struct {
				Kind    string `json:"kind"`
				Payload struct {
					Output string `json:"output"`
				} `json:"payload"`
			}
			if json.Unmarshal([]byte(raw), &e) == nil && e.Kind == "interactive" && e.Payload.Output != "" {
				kept <- e.Payload.Output
				return
			}
		}
	}()

	if ok, msg := runner.SendInteractive("a white cat with a red hat"); !ok {
		t.Fatalf("send a prompt: %s", msg)
	}

	_, outputs, err := cfg.SessionDirs(cfg.CurrentSession())
	if err != nil {
		t.Fatal(err)
	}
	takes := waitForPNGs(t, outputs, 1, 20*time.Second)

	// The bytes are the ones iris made, not an empty file.
	got, err := os.ReadFile(takes[0])
	if err != nil || string(got) != "a generated image" {
		t.Fatalf("the take is not what iris wrote: %q, %v", got, err)
	}
	// A sidecar beside it, as every other take has.
	side := strings.TrimSuffix(takes[0], ".png") + ".json"
	var meta map[string]any
	raw, err := os.ReadFile(side)
	if err != nil {
		t.Fatalf("no sidecar: %v", err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	params, _ := meta["params"].(map[string]any)
	if params["prompt"] != "a white cat with a red hat" {
		t.Errorf("the sidecar does not say what was asked for: %v", params["prompt"])
	}
	if params["source"] != "terminal" {
		t.Errorf("the sidecar does not say where the take came from: %v", params["source"])
	}
	// The rail labels a take "seed N · WxH" from these, and showed "? · ?x?"
	// while they went unread — the REPL prints both before it prints Done.
	if meta["seed"] != float64(4242) {
		t.Errorf("seed = %v; iris printed 4242", meta["seed"])
	}
	if params["width"] != float64(512) || params["height"] != float64(512) {
		t.Errorf("size = %vx%v; iris printed 512x512", params["width"], params["height"])
	}
	// And the session's history, which is what the takes rail reads.
	settings := readJSONObject(cfg.SessionSetting(cfg.CurrentSession()))
	rows, _ := settings["takes"].([]any)
	if len(rows) != 1 {
		t.Fatalf("the session recorded %d takes; want 1", len(rows))
	}
	// And the page is told which take to show, since no finished job will.
	if named := <-kept; named != filepath.Base(takes[0]) {
		t.Errorf("the viewer was told to show %q; the take is %q", named, filepath.Base(takes[0]))
	}
}

// The reader must not wedge when no render is waiting for the REPL's lines.
// The channel it used to post every line to is only drained by a render, and
// a generation writes hundreds — two of them would fill it and the terminal
// would go quiet for good.
func TestTheTerminalKeepsReadingWithNoRenderWaiting(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	if ok, msg := runner.LoadInteractive(map[string]any{"session_name": "typed"}); !ok {
		t.Fatalf("load interactive iris: %s", msg)
	}
	// Comfortably more lines than the channel holds.
	for i := 0; i < 4; i++ {
		if ok, msg := runner.SendInteractive(fmt.Sprintf("!noise %d", i)); !ok {
			t.Fatalf("send %d: %s", i, msg)
		}
	}
	if ok, msg := runner.SendInteractive("a cat"); !ok {
		t.Fatalf("send the prompt: %s", msg)
	}
	_, outputs, err := cfg.SessionDirs(cfg.CurrentSession())
	if err != nil {
		t.Fatal(err)
	}
	// It only gets here if the reader was still reading.
	waitForPNGs(t, outputs, 1, 20*time.Second)
}

// The reference job the old poll got wrong: iris's ids start at 1 and count
// images loaded and generated alike, the !load writes a copy of the reference
// beside the images it generates, and only the panel's size may reach the
// render. The stub makes its image from the reference it was pointed at, so
// the bytes say which one that was.
func TestAReferenceJobTakesItsOwnImageAtThePanelSize(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	addRef(t, cfg, "first.png", "the first")
	addRef(t, cfg, "second.png", "the second")

	takes := []struct{ prompt, ref, want string }{
		{"into a fresh REPL", "first.png", "made from the first at 256x256"},
		{"no reference", "", "a generated image"},
		{"after an image the REPL made", "second.png", "made from the second at 256x256"},
		{"one it already has", "first.png", "made from the first at 256x256"},
	}
	for _, take := range takes {
		var refs []string
		if take.ref != "" {
			refs = []string{take.ref}
		}
		job := interactiveJob(t, runner, take.prompt, refs...)
		if job.state() != "done" {
			t.Fatalf("%s: %v", take.prompt, job.Summary())
		}
		got, err := os.ReadFile(job.outputPath())
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != take.want {
			t.Errorf("%s: the take is %q", take.prompt, got)
		}
	}
}

// An engine error has to end the render, and it can arrive stuck to the frame
// before it. The stub refuses a reference called bad.png the way iris refuses
// a format it cannot read, and that !load takes no id, so the reference loaded
// after it does not get the id the studio expected.
func TestAnEngineErrorEndsTheRenderAndTakesNoID(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	addRef(t, cfg, "bad.png", "not an image")
	addRef(t, cfg, "ref.png", "the reference")
	if job := interactiveJob(t, runner, "a plain take leaves a frame"); job.state() != "done" {
		t.Fatalf("plain job: %v", job.Summary())
	}

	job := interactiveJob(t, runner, "this cannot work", "bad.png", "ref.png")
	if state, failure := job.outcome(); state != "failed" || !strings.Contains(failure, "Cannot load") {
		t.Fatalf("state %q, error %q", state, failure)
	}
	if lines := strings.Join(job.Summary()["log"].([]string), "\n"); !strings.Contains(lines, "Cannot load") {
		t.Errorf("the error is not in the log: %q", lines)
	}

	job = interactiveJob(t, runner, "this can", "ref.png")
	if job.state() != "done" {
		t.Fatalf("the job after the failure: %v", job.Summary())
	}
	if got, _ := os.ReadFile(job.outputPath()); string(got) != "made from the reference at 256x256" {
		t.Errorf("the take after the failure is %q", got)
	}

	// Tried again, it must not be mistaken for the image that took its id.
	job = interactiveJob(t, runner, "this still cannot work", "bad.png")
	if state, failure := job.outcome(); state != "failed" || !strings.Contains(failure, "Cannot load") {
		t.Fatalf("the retry: state %q, error %q", state, failure)
	}
}

// Cancel stops the REPL, because that is all an interactive render has, and
// does not wait on iris to get there.
func TestCancelStopsAnInteractiveRender(t *testing.T) {
	_, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	job := runner.Submit(interactiveParams("hang"))
	for job.state() != "running" || !runner.awaitingImage() {
		time.Sleep(5 * time.Millisecond)
	}
	if !runner.Cancel(job.ID) {
		t.Fatal("Cancel refused a running interactive render")
	}
	if state := jobEnds(t, job); state != "cancelled" {
		t.Fatalf("state %q: %v", state, job.Summary())
	}
}

// With no render waiting, the reader's nil used to stay in the channel and fail
// the next render the moment it looked.
func TestAGenerateAfterUnloadIsNotToldIrisExited(t *testing.T) {
	_, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	if ok, msg := runner.LoadInteractive(map[string]any{"session_name": "typed"}); !ok {
		t.Fatalf("load interactive iris: %s", msg)
	}
	runner.StopInteractive()
	if job := interactiveJob(t, runner, "a take"); job.state() != "done" {
		t.Fatalf("job after Unload: %v", job.Summary())
	}
}

// The page disables Generate while the REPL says it is busy, so busy has to
// mean a generation: not startup's "Loading VAE...", and it has to end. Only
// the first false is awaited, so Load's true, had it fired, would be counted.
func TestThePageIsBusyOnlyWhileTheReplGenerates(t *testing.T) {
	_, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	events := runner.Subscribe()
	defer runner.Unsubscribe(events)
	if ok, msg := runner.LoadInteractive(map[string]any{"session_name": "typed"}); !ok {
		t.Fatalf("load interactive iris: %s", msg)
	}
	if ok, msg := runner.SendInteractive("!explore 2 a green frog"); !ok {
		t.Fatalf("send: %s", msg)
	}
	var busy []bool
	timeout := time.After(20 * time.Second)
	for {
		select {
		case raw := <-events:
			var e struct {
				Kind    string `json:"kind"`
				Payload struct {
					Running bool `json:"running"`
				} `json:"payload"`
			}
			if json.Unmarshal([]byte(raw), &e) != nil || e.Kind != "interactive" {
				continue
			}
			busy = append(busy, e.Payload.Running)
			if !e.Payload.Running {
				if len(busy) != 2 {
					t.Errorf("busy events %v; want one true, then false", busy)
				}
				return
			}
		case <-timeout:
			t.Fatalf("busy never cleared: %v", busy)
		}
	}
}

// A random seed is resolved here: iris picks one to print and another to
// render, so the one it reports only reproduces the image if it was given.
func TestARandomSeedIsChosenBeforeIrisRuns(t *testing.T) {
	cfg := &Config{Iris: "iris", Model: "model"}
	seedArg := func(seed any) string {
		args := buildOneshotArgs(cfg, map[string]any{"seed": seed}, "in", "out.png", false)
		for i, arg := range args {
			if arg == "-S" {
				return args[i+1]
			}
		}
		return ""
	}
	if got := seedArg(float64(7)); got != "7" {
		t.Errorf("fixed seed sent as %q", got)
	}
	if got, err := strconv.ParseInt(seedArg(float64(-1)), 10, 64); err != nil || got < 0 {
		t.Errorf("seed -1 sent as %d, %v; want a real seed", got, err)
	}
}

// A one-shot failure says what iris said, not just that it stopped.
func TestAOneShotFailureSaysWhy(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "iris")
	script := "#!/bin/sh\necho 'Error: Image dimensions exceed maximum' >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewConfig(Args{Iris: stub, Model: fakeModelDir(t), Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	job := NewRunner(cfg).Submit(map[string]any{"session_name": "s", "prompt": "a", "run_mode": "oneshot"})
	jobEnds(t, job)
	if _, failure := job.outcome(); failure != "Image dimensions exceed maximum" {
		t.Errorf("error %q", failure)
	}
}

// interactiveStudio is a studio whose iris is a stub REPL: it reads lines,
// writes an image into a temp directory of its own for anything that is not a
// ! command, and announces it the way iris_cli.c does. Ids start at 1 and are
// shared by images loaded and generated. Its frame ends with no newline, which
// is how an iris without line-buffered stdout leaves one, so whatever comes
// next is stuck to it.
func interactiveStudio(t *testing.T) (*Config, *Runner) {
	t.Helper()
	model := t.TempDir()
	for _, sub := range []string{"transformer", "vae"} {
		if err := os.MkdirAll(filepath.Join(model, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tmp := t.TempDir()
	stub := filepath.Join(t.TempDir(), "iris")
	script := `#!/bin/sh
tmp="` + tmp + `"
frame() { printf '\033_Gf=24,s=1,v=1,m=0;AAAA\033\\'; }
n=0
echo "Seed: 1793450176"
echo "Loading VAE... done (0.4s)"
while IFS= read -r line; do
  case "$line" in
    "!load "*)
      src="${line#!load }"
      case "$src" in
        *bad.png) frame; echo "Error: Cannot load '$src'" >&2; continue ;;
      esac
      n=$((n+1))
      cp "$src" "$tmp/image-000$n.png"
      echo "Loaded: $src (64x64, ref \$$n)"
      ;;
    "!explore"*)
      echo "Generating 2 images at 256x256..."
      printf '  [1/2] Seed: 1 '; frame; echo
      printf '  [2/2] Seed: 2 '; frame; echo
      echo "Done. Use !seed <n> then re-run prompt at full size."
      ;;
    "!"*) echo "ack $line" ;;
    hang*) echo "Generating 512x512..."; read -r _; exit 0 ;;
    *)
      ref=""
      size=""
      case "$line" in
        '$'*)
          set -f; set -- $line; set +f
          while [ "${1#?}" != "$1" ] && [ "${1%${1#?}}" = '$' ]; do
            ref="$tmp/image-000${1#?}.png"
            if [ ! -f "$ref" ]; then
              echo "Error: Reference $1 not found." >&2
              continue 2
            fi
            shift
          done
          size="$1"
          ;;
      esac
      echo "Seed: 4242"
      echo "Generating 512x512..."
      # 300 lines of noise, as a real generation writes.
      i=0; while [ $i -lt 300 ]; do echo "[1/4]:dddd"; i=$((i+1)); done
      n=$((n+1))
      out="$tmp/image-000$n.png"
      if [ -n "$ref" ]; then
        printf 'made from %s at %s' "$(cat "$ref")" "$size" > "$out"
      else
        printf 'a generated image' > "$out"
      fi
      echo "Done -> $out (ref \$$n) [1.00s]"
      frame
      ;;
  esac
done
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewConfig(Args{Iris: stub, Model: model, Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, NewRunner(cfg)
}

func interactiveParams(prompt string, refs ...string) map[string]any {
	list := []any{}
	for _, name := range refs {
		list = append(list, map[string]any{"name": name})
	}
	return map[string]any{
		"session_name": "refs", "prompt": prompt, "run_mode": "interactive",
		"width": 256, "height": 256, "seed": -1, "refs": list,
	}
}

// interactiveJob queues a render and waits for it to end.
func interactiveJob(t *testing.T, runner *Runner, prompt string, refs ...string) *Job {
	t.Helper()
	job := runner.Submit(interactiveParams(prompt, refs...))
	jobEnds(t, job)
	return job
}

func jobEnds(t *testing.T, job *Job) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		state := job.state()
		if state == "done" || state == "failed" || state == "cancelled" {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("job still %q after 20s: %v", state, job.Summary())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func addRef(t *testing.T, cfg *Config, name, content string) {
	t.Helper()
	inputs, _, err := cfg.SessionDirs("refs")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputs, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitForPNGs(t *testing.T, dir string, want int, within time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got, _ := filepath.Glob(filepath.Join(dir, "*.png"))
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s holds %d png(s) after %s; want %d", dir, len(got), within, want)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
