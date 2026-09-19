package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A render writes a job's fields from the runner's goroutine while /api/queue
// reads them from whichever goroutine is serving, so every one of them goes
// through Job.set and Job.Summary. Under -race this fails the moment one
// does not.
//
// Nothing here touches a real model or a real iris: the engine is a stub
// script that prints what iris prints and copies a fixture into place, and
// everything the studio keeps is under t.TempDir().
func TestTheQueueCanBeReadWhileARenderWritesToIt(t *testing.T) {
	root := t.TempDir()
	model := fakeModelDir(t)
	iris := stubIris(t, fixturePNG(t))

	cfg, err := NewConfig(Args{Iris: iris, Model: model, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	// No platform: this is the studio on its own, and every call on a nil one
	// is a no-op (server/helmstudio.go).
	if cfg.Platform != nil {
		t.Fatal("a test must not reach a platform")
	}
	runner := NewRunner(cfg)
	defer runner.StopInteractive()
	srv := httptest.NewServer(NewApp(cfg, runner))
	defer srv.Close()

	// Readers first, so they are already in Summary when the writes start.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				get(t, srv.URL+"/api/queue")
			}
		}()
	}

	body := map[string]any{
		"session_name": "race", "label": "race", "prompt": "a stub render",
		"width": 256, "height": 256, "steps": 4, "seed": 7, "guidance": 0,
		"schedule": "default", "power_alpha": 2.0, "base_mode": false, "mmap": true,
		"run_mode": "oneshot", "show_steps": false, "zoom": 2, "refs": []any{},
	}
	raw, _ := json.Marshal(body)
	res, err := http.Post(srv.URL+"/api/render", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var submitted map[string]any
	if err := json.NewDecoder(res.Body).Decode(&submitted); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	id, _ := submitted["id"].(string)
	if id == "" {
		t.Fatalf("no job came back from /api/render: %v", submitted)
	}

	state := waitForJob(t, srv.URL, id, 30*time.Second)
	close(stop)
	readers.Wait()

	if state["state"] != "done" {
		t.Fatalf("the render did not finish: state %v, error %v", state["state"], state["error"])
	}
	if state["output"] == nil || state["output"] == "" {
		t.Errorf("a finished render named no output: %v", state)
	}
	// What the runner parsed out of the stub's lines, which is what it parses
	// out of iris's.
	if seed, _ := state["seed"].(float64); seed != 42 {
		t.Errorf("seed = %v; the stub printed 42", state["seed"])
	}
	if lines, _ := state["log"].([]any); len(lines) == 0 {
		t.Error("the job kept no log")
	}
}

// waitForJob polls /api/queue until the job leaves the queue, and returns it.
func waitForJob(t *testing.T, base, id string, within time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		var page struct {
			Queue   []map[string]any `json:"queue"`
			History []map[string]any `json:"history"`
		}
		if err := json.Unmarshal(get(t, base+"/api/queue"), &page); err != nil {
			t.Fatal(err)
		}
		for _, job := range page.History {
			if job["id"] == id {
				return job
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never finished", id)
	return nil
}

func get(t *testing.T, url string) []byte {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(res.Body); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeModelDir is what isModelDir looks for: transformer/ and vae/ beside
// each other. It holds no weights and nothing reads one.
func fakeModelDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"transformer", "vae"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fixturePNG is a real one-pixel PNG, so the sidecar and the outputs listing
// see a file of the kind they expect.
func fixturePNG(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0xd8, G: 0x55, B: 0x8f, A: 0xff})
	path := filepath.Join(t.TempDir(), "fixture.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubIris prints what iris prints — the seed line, the step lines the runner
// parses progress from, and enough of them that the runner is still writing
// while the pollers read — then puts the fixture where -o asked for it.
func stubIris(t *testing.T, fixture string) string {
	t.Helper()
	var steps strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&steps, "echo \"[%d/200]: denoising\"\n", i)
	}
	script := `#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
echo "MPS: Metal GPU | stub"
echo "Seed: 42"
` + steps.String() + `echo "Decoding image... done"
cp "` + fixture + `" "$out"
echo "Saving... $out"
`
	path := filepath.Join(t.TempDir(), "iris")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
