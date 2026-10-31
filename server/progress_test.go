package server

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

// What iris actually writes, and what must not be mistaken for it. Every
// line here is copied from the source named beside it, not invented.
func TestProgressIsReadFromWhatIrisPrints(t *testing.T) {
	cases := []struct {
		line     string
		num, den int
		from     string
	}{
		// iris.c/main.c, cli_step_callback: the one-shot CLI, which is what
		// the studio runs when it passes -p and -o.
		{"  Step 2/4 ddddddddssssF", 2, 4, "one-shot, mid-step"},
		{"  Step 1/4 dddddddd", 1, 4, "one-shot, part-way through a step"},
		// pump trims the trailing space iris writes, so the last step of a
		// render arrives with nothing after the total.
		{"  Step 4/4", 4, 4, "one-shot, trailing space trimmed"},
		{"  Step 12/50 d", 12, 50, "one-shot, two digits"},
		// FLUX.3 prints no d/s/F, and Apple's Metal stack writes its own line
		// onto the end of the first step's.
		{"  Step 1/4 Failed to import MPS module", 1, 4, "one-shot, FLUX.3 first step"},

		// iris.c/iris_cli.c, cli_step_progress: the REPL, which is what
		// interactive mode drives.
		{"[2/4]:ddsssf", 2, 4, "REPL, mid-step"},
		{"[1/4]:", 1, 4, "REPL, step just started"},
		{"[12/50]:d", 12, 50, "REPL, two digits"},
		{"[1/4]:Failed to import MPS module", 1, 4, "REPL, FLUX.3 first step"},
	}
	for _, c := range cases {
		num, den, ok := stepProgress(c.line)
		if !ok || num != c.num || den != c.den {
			t.Errorf("%s: stepProgress(%q) = %d, %d, %v; want %d, %d, true",
				c.from, c.line, num, den, ok, c.num, c.den)
		}
	}

	never := []struct{ line, why string }{
		// iris.c/iris_sample.c: the timing breakdown, printed after the
		// render. Reading these as progress would wind the bar backwards
		// over a render that has already finished.
		{"  Step 1: 1759.6 ms", "the timing breakdown"},
		{"  Step 4: 945.7 ms", "the timing breakdown"},
		// iris.c/iris_cli.c: !explore counts images, not denoising steps.
		{"  [1/4] Seed: 4242 ", "!explore's per-image counter"},
		// iris.c/main.c and iris_cli.c: the step-image marker --show-steps
		// prints, which stepMarkRe reads and this must not.
		{"[Step 3]", "the --show-steps marker"},
		{"Denoising (d=double block, s=single blocks, F=final):", "the legend"},
		// A prompt echoed back, which is why the one-shot pattern is anchored.
		{"Prompt: a cat on Step 2/4 of a staircase", "a prompt that says Step 2/4"},
		{"Total generation time: 23.8 seconds", "an ordinary line"},
	}
	for _, c := range never {
		if num, den, ok := stepProgress(c.line); ok {
			t.Errorf("%s: stepProgress(%q) = %d, %d, true; want no match", c.why, c.line, num, den)
		}
	}
}

// fed runs lines through a fresh set of stages, one second apart, and leaves
// the clock on the last of them.
func fed(lines ...string) (stages, float64) {
	s := stages{}
	now := 100.0
	for _, line := range lines {
		now++
		s.read(line, now)
	}
	return s, now
}

// A real run of iris.c/iris at 256x256 and 4 steps, as pump splits it. What
// follows "..." on a line is whatever iris wrote before it said done.
var oneShotRun = []string{
	"Prompt: a red cube on a table",
	"Loading VAE...  Using mmap mode for text encoder (lower memory)",
	" done (0.2s)",
	"Model: FLUX.3-image-klein v1.0 (distilled, 16 steps, guidance 0.0)",
	"Loading Qwen3 encoder... Qwen3 tokenizer loaded (151669 vocab)",
	"Qwen3: bf16 GPU acceleration enabled",
	"Mmap mode: layer weights will be loaded on-demand",
	" done (0.2s)",
	"Encoding text... done (0.8s)",
	"Loading FLUX.3 transformer...  FLUX.3 transformer: dim=3072 heads=24 blocks=5x3+28 mlp=9216 (flow map) on GPU",
	" done (1.4s)",
	"Denoising (d=double block, s=single blocks, F=final):",
	"  Step 1/4 Failed to import MPS module",
	"Failed to import MPS module",
	"Failed to import MPS module",
	"  Step 2/4",
	"  Step 3/4",
	"  Step 4/4",
	"Denoising timing breakdown (FLUX.3):",
	"  Step 1: 245.8 ms",
	"  Total denoising: 597.8 ms (0.60 s)",
	"Decoding image... done (0.2s)",
	"Generated in 3.3s total",
	"  Output: 256x256, 3 channels",
	"Saving... /tmp/take.png 256x256 (0.0s)",
	"Total generation time: 3.5 seconds",
}

func TestAOneShotRenderIsTimedByWhatIrisSaysItTook(t *testing.T) {
	s, _ := fed(oneShotRun...)
	// Load comes round three times and its time is all three together.
	if got := s["Loading"].Seconds; math.Abs(got-1.8) > 1e-9 {
		t.Errorf("Loading took %v; iris said 0.2, 0.2 and 1.4", got)
	}
	want := map[string]float64{"Encoding": 0.8, "Decoding": 0.2, "Saving": 0}
	for phase, seconds := range want {
		if s[phase].State != "done" || s[phase].Seconds != seconds {
			t.Errorf("%s = %+v; want done in %v", phase, s[phase], seconds)
		}
	}
	// iris does not say how long denoising took in these words, so the clock
	// does: from the legend to Decoding, ten lines of fed.
	if got := s["Denoising"]; got.State != "done" || got.Seconds != 10 {
		t.Errorf("Denoising = %+v; want done in 10 by the clock", got)
	}
}

// A real line from the REPL's reader, whose phase lines end in a newline and
// carry the whole name again when they end (iris.c/iris_cli.c).
func TestAReplWithANewPromptShowsTheEncoderItLoadedAgain(t *testing.T) {
	s, _ := fed("Generating 256x256...",
		"Loading Qwen3 encoder...", "Loading Qwen3 encoder... done (0.1s)",
		"Encoding text...", "ttttttttttttttttttttttttttttttttt", "Encoding text... done (0.8s)",
		"[1/4]:", "[2/4]:", "[3/4]:", "[4/4]:",
		"Decoding image...", "vvvvvvvvvvvvvvv", "Decoding image... done (0.1s)",
		"Done -> /tmp/iris-lO4ctv/image-0004.png (ref $4) [2.19s]")
	if s["Loading"].State != "done" || s["Encoding"].State != "done" {
		t.Errorf("stages = %+v; a new prompt loads and encodes", s)
	}
	// What is left of a render after Done is copying the take out, which the
	// worker ends when the file is there.
	if s["Saving"].State != "now" {
		t.Errorf("Saving = %+v; Done is when the studio starts saving", s["Saving"])
	}
}

func TestACachedEmbeddingSkipsLoadAndEncode(t *testing.T) {
	s, _ := fed("Seed: 42", "Generating 256x256...", "(using cached embedding)", "[1/4]:")
	for _, phase := range []string{"Loading", "Encoding"} {
		if s[phase].State != "skip" {
			t.Errorf("%s = %+v; iris went straight to denoising", phase, s[phase])
		}
	}
	if s["Denoising"].State != "now" {
		t.Errorf("Denoising = %+v; want now", s["Denoising"])
	}
}

// The REPL encodes a reference before it loads anything, so Load is not behind
// the render yet when Encode lights.
func TestAStageUnseenBeforeDenoisingIsNotYetSkipped(t *testing.T) {
	s, _ := fed("Generating at reference size (img2img)...",
		"Encoding reference images...", "Encoding reference images... done (0.2s)")
	if _, seen := s["Loading"]; seen {
		t.Errorf("Loading = %+v; it is still to come", s["Loading"])
	}
	s.read("Loading Qwen3 encoder...", 200)
	if s["Loading"].State != "now" || s["Encoding"].State != "done" {
		t.Errorf("stages = %+v; a stage is left when the next one starts", s)
	}
}

func TestAPromptIrisEchoesIsNotAStage(t *testing.T) {
	s, _ := fed("Prompt: Loading VAE...", "Prompt: Saving... done (3.0s)", "Output: /tmp/Decoding... (9.0s)")
	if len(s) != 0 {
		t.Errorf("stages = %+v; want none", s)
	}
}

// A real one-shot whose save failed, and one stopped with SIGTERM part-way
// through encoding: neither prints another stage, and neither may leave one lit.
func TestARenderThatStopsLeavesNoStageLit(t *testing.T) {
	for _, c := range []struct {
		state, phase string
		lines        []string
	}{
		{"failed", "Saving", []string{"Saving...", "Error: Failed to save image: /tmp/nodir/x.png"}},
		{"cancelled", "Encoding", []string{"Encoding text..."}},
	} {
		s, now := fed(c.lines...)
		s.leave(c.state, -1, now+1)
		for phase, mark := range s {
			if mark.State == "now" {
				t.Errorf("%s: %s is still lit", c.state, phase)
			}
		}
		if s[c.phase].State != c.state {
			t.Errorf("%s = %+v; want %s", c.phase, s[c.phase], c.state)
		}
	}
}

// iris writes "Encoding text..." and goes quiet until it has finished, and pump
// has only ever handed on whole lines.
func TestALineIrisHasNotFinishedLightsItsStage(t *testing.T) {
	runner := NewRunner(&Config{Sessions: t.TempDir()})
	job := newJob(map[string]any{"session_name": "s"})
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		runner.pump(job, reader)
	}()
	stageOf := func(phase string) string {
		summary := job.Summary()
		return summary["stages"].(stages)[phase].State
	}
	wait := func(phase, state string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); stageOf(phase) != state; {
			if time.Now().After(deadline) {
				t.Fatalf("%s = %q; want %q", phase, stageOf(phase), state)
			}
			time.Sleep(time.Millisecond)
		}
	}
	_, _ = writer.WriteString("Encoding text...")
	wait("Encoding", "now")
	if phase := job.Summary()["phase"]; phase != "Encoding" {
		t.Errorf("phase = %v; want Encoding", phase)
	}
	_, _ = writer.WriteString(" done (0.8s)\n")
	wait("Encoding", "done")
	_ = writer.Close()
	<-pumped
	if got := job.Summary()["stages"].(stages)["Encoding"].Seconds; got != 0.8 {
		t.Errorf("Encoding took %v; iris said 0.8", got)
	}
}

// A render typed into the terminal has no job, so what the page needs to draw
// it, on reload as on any other line, has to travel with the REPL's activity.
func TestATerminalRenderCarriesItsStages(t *testing.T) {
	runner := NewRunner(&Config{Sessions: t.TempDir()})
	events := runner.Subscribe()
	defer runner.Unsubscribe(events)
	runner.interactiveActivity("Generating 256x256...")
	runner.readStage("Loading Qwen3 encoder...")
	var last struct {
		Payload struct {
			Phase   string
			Started float64
			Stages  stages
		}
	}
	for len(events) > 0 {
		if err := json.Unmarshal([]byte(<-events), &last); err != nil {
			t.Fatal(err)
		}
	}
	if last.Payload.Phase != "Loading" || last.Payload.Stages["Loading"].State != "now" || last.Payload.Started == 0 {
		t.Errorf("activity = %+v; want Loading lit, and when it began", last.Payload)
	}
	// Only a generation has stages: what the REPL says as it starts is not one.
	runner.interactiveBusy(false, "", nil)
	runner.readStage("Loading VAE...")
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.replBusy {
		t.Error("a line the REPL printed while idle made it busy")
	}
}
