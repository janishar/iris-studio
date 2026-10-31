package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// The rail labels a take "seed N · WxH" and showed "? · ?x?" while the seed
	// went unread — the REPL prints it before it prints Done.
	if meta["seed"] != float64(4242) {
		t.Errorf("seed = %v; iris printed 4242", meta["seed"])
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

// iris takes a line that starts with ! as a command, one that starts with $ as
// a reference, and a size at either end as the size. A render's prompt is none
// of those, so the panel's size goes first on the line, and iris reads what is
// behind it as text. The stub stalls or errors on the first two if that slips;
// it reads no sizes, so the line itself is checked for the third.
func TestAPromptThatLooksLikeARenderInstructionStaysAPrompt(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	addRef(t, cfg, "ref.png", "the reference")

	takes := []struct {
		prompt string
		refs   []string
		line   string
	}{
		{"$ and ! alike", []string{"ref.png"}, "$1 256x256 $ and ! alike"},
		{"!quit the old ways", nil, "256x256 !quit the old ways"},
		{"$9 million dollars", nil, "256x256 $9 million dollars"},
		{"a wide view 1792x1024", nil, "256x256 a wide view 1792x1024"},
	}
	for _, take := range takes {
		job := interactiveJob(t, runner, take.prompt, take.refs...)
		if job.state() != "done" {
			t.Fatalf("%q: %v", take.prompt, job.Summary())
		}
		command := job.Summary()["command"].([]string)
		if got := command[len(command)-1]; got != take.line {
			t.Errorf("%q reached iris as %q", take.prompt, got)
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

// A failed !load has the rest of its render behind it in iris's stdin, and the
// next render used to start under that. J1's prompt names an id the failed !load
// never got, so iris's answer to it was an error, and J2 took it for its own.
func TestAFailedRenderTakesTheRestOfItsAnswersWithIt(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	addRef(t, cfg, "bad.png", "not an image")
	addRef(t, cfg, "slow.png", "a large image")
	first, err := runner.Submit(interactiveParams("this cannot work", "bad.png", "slow.png"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := runner.Submit(interactiveParams("this can"))
	if err != nil {
		t.Fatal(err)
	}
	if state := jobEnds(t, first); state != "failed" {
		t.Fatalf("the first job: %v", first.Summary())
	}
	if state := jobEnds(t, second); state != "done" {
		t.Fatalf("the job behind it: %v", second.Summary())
	}
}

// A REPL that dies takes one render with it. The ones behind used to find it
// still counted as loaded, write to a dead pipe and fail, and so emptied the queue.
func TestTheRendersBehindADeadReplLoadItAgain(t *testing.T) {
	_, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	dies, err := runner.Submit(interactiveParams("oom die"))
	if err != nil {
		t.Fatal(err)
	}
	behind := []*Job{}
	for _, prompt := range []string{"the next", "the one after"} {
		job, err := runner.Submit(interactiveParams(prompt))
		if err != nil {
			t.Fatal(err)
		}
		behind = append(behind, job)
	}
	if state := jobEnds(t, dies); state != "failed" {
		t.Fatalf("state %q: %v", state, dies.Summary())
	}
	for _, job := range behind {
		if state := jobEnds(t, job); state != "done" {
			t.Errorf("state %q: %v", state, job.Summary())
		}
	}
}

// Cancel stops the REPL, because that is all an interactive render has, and
// does not wait on iris to get there.
func TestCancelStopsAnInteractiveRender(t *testing.T) {
	_, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	job, err := runner.Submit(interactiveParams("hang"))
	if err != nil {
		t.Fatal(err)
	}
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
					Running *bool `json:"running"`
				} `json:"payload"`
			}
			// The word that a REPL has started says nothing of running.
			if json.Unmarshal([]byte(raw), &e) != nil || e.Kind != "interactive" || e.Payload.Running == nil {
				continue
			}
			busy = append(busy, *e.Payload.Running)
			if !*e.Payload.Running {
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
	cfg := &Config{Iris: "iris", Model: "model", Sessions: t.TempDir()}
	runner := &Runner{cfg: cfg}
	for _, mode := range []string{"oneshot", "interactive"} {
		job := newJob(map[string]any{"session_name": "s", "seed": float64(-1), "run_mode": mode})
		if !runner.settleSeeds(job) {
			t.Fatalf("%s: no seed: %v", mode, job.Error)
		}
		if *job.Seed < 0 || *job.noise != *job.Seed {
			t.Errorf("%s: base %d, noise %d; want one real seed", mode, *job.Seed, *job.noise)
		}
		if job.Params["seed"] != float64(-1) {
			t.Errorf("%s: the seed as typed became %v", mode, job.Params["seed"])
		}
		args := buildOneshotArgs(cfg, job.renderParams(), "in", "out.png", false)
		if got := args[slices.Index(args, "-S")+1]; got != strconv.FormatInt(*job.Seed, 10) {
			t.Errorf("%s: sent %q, base is %d", mode, got, *job.Seed)
		}
	}
}

// Editing a take with its own seed must not restart from the take's noise, and
// the same seed and references must still reproduce it.
func TestAReferenceGivesFreshNoiseThatTheSameInputsReproduce(t *testing.T) {
	dir := t.TempDir()
	path := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := path("a.png", "one image"), path("b.png", "another image")
	noise := func(base int64, refs ...string) int64 {
		t.Helper()
		v, err := noiseSeed(base, refs)
		if err != nil {
			t.Fatal(err)
		}
		if v < 0 || v >= 1<<53 {
			t.Errorf("noise %d does not survive the page's numbers", v)
		}
		return v
	}
	if noise(7) != 7 {
		t.Error("a render with no references should use its seed as it is")
	}
	first := noise(7, a, b)
	if first == 7 {
		t.Error("references did not change the noise")
	}
	if noise(7, a, b) != first {
		t.Error("the same seed and references gave another value")
	}
	if noise(7, a) == first || noise(7, b, a) == first || noise(8, a, b) == first {
		t.Error("another reference, order or seed gave the same value")
	}
	if _, err := noiseSeed(7, []string{filepath.Join(dir, "gone.png")}); err == nil {
		t.Error("a reference that is gone should stop the render")
	}
}

// What iris prints as the seed is the noise once there are references, and the
// page and the sidecar go on saying the one the user can reuse.
func TestTheSidecarRecordsTheBaseSeedAndTheNoise(t *testing.T) {
	cfg := planConfig(t, flux3Config)
	addImage(t, cfg, "a.png", 64, 64)
	runner := &Runner{cfg: cfg}
	job := newJob(map[string]any{"session_name": "refs", "seed": float64(7), "refs": refsOf("a.png")})
	if !runner.settleSeeds(job) {
		t.Fatalf("no seed: %v", job.Error)
	}
	want, err := noiseSeed(7, []string{filepath.Join(cfg.Sessions, "refs", "inputs", "a.png")})
	if err != nil {
		t.Fatal(err)
	}
	summary := job.Summary()
	if summary["seed"] != int64(7) || summary["noise_seed"] != want {
		t.Errorf("seed %v, noise %v; want 7 and %d", summary["seed"], summary["noise_seed"], want)
	}
	if got := job.renderParams()["seed"]; got != want {
		t.Errorf("iris would be sent %v, want %d", got, want)
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
	job, err := NewRunner(cfg).Submit(map[string]any{"session_name": "s", "prompt": "a", "run_mode": "oneshot"})
	if err != nil {
		t.Fatal(err)
	}
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
// next is stuck to it. A prompt ending in "hold" generates until a file called
// release appears in cfg.Workdir, because a typed line cannot reach it.
func interactiveStudio(t *testing.T) (*Config, *Runner) {
	t.Helper()
	fixture := fixturePNG(t)
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
        *slow.png) sleep 0.3 ;;
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
    "!size") echo "Size: 256x256" ;;
    "!"*) echo "ack $line" ;;
    *" hang") echo "Generating 512x512..."; read -r _; exit 0 ;;
    *" die") echo "Generating 512x512..."; exit 1 ;;
    *" hold")
      echo "Generating at reference size (img2img)..."
      while [ ! -e release ]; do sleep 0.01; done
      rm release
      n=$((n+1))
      out="$tmp/image-000$n.png"
      cp "` + fixture + `" "$out"
      echo "Done -> $out (ref \$$n) [1.00s]"
      ;;
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
	job, err := runner.Submit(interactiveParams(prompt, refs...))
	if err != nil {
		t.Fatal(err)
	}
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
