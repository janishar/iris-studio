package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// interactiveStudio is a studio whose iris is a stub REPL: it reads lines,
// writes an image into a temp directory of its own for anything that is not a
// ! command, and announces it the way iris_cli.c does.
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
n=0
while IFS= read -r line; do
  case "$line" in
    "!"*) echo "ack $line" ;;
    *)
      n=$((n+1))
      out="` + tmp + `/image-000$n.png"
      printf 'a generated image' > "$out"
      # 300 lines of noise, as a real generation writes.
      i=0; while [ $i -lt 300 ]; do echo "[1/4]:dddd"; i=$((i+1)); done
      echo "Generating 512x512..."
      echo "Seed: 4242"
      echo "Done -> $out (ref \$$n) [1.00s]"
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
