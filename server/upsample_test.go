package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// upsampleStudio is a FLUX.3 studio with a pup checkpoint beside it and pupBody
// as its Python, which sees the request as request.json in its working
// directory. Every run of it leaves a line in runs. iris copies the fixture and
// keeps what it was told in argv.
type upsampleKit struct {
	cfg    *Config
	runner *Runner
	runs   string
	argv   string
}

func upsampleStudio(t *testing.T, pupBody string) upsampleKit {
	t.Helper()
	dir := t.TempDir()
	model := filepath.Join(dir, "flux-3-image-klein")
	for _, sub := range []string{"transformer", "vae"} {
		if err := os.MkdirAll(filepath.Join(model, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(model, "transformer", "config.json"), flux3Config, 0o644)
	addPupCheckpoint(t, filepath.Join(dir, "flux-3-image-klein-pup"))

	kit := upsampleKit{runs: filepath.Join(dir, "runs"), argv: filepath.Join(dir, "argv")}
	pup := filepath.Join(dir, "pup")
	writeFile(t, filepath.Join(pup, ".venv", "bin", "python"), `#!/bin/sh
cat > request.json
echo run >> "`+kit.runs+`"
`+pupBody, 0o755)
	iris := filepath.Join(dir, "iris")
	writeFile(t, iris, `#!/bin/sh
printf '%s\n' "$@" > "`+kit.argv+`"
while [ $# -gt 0 ]; do
  [ "$1" = -o ] && cp "`+fixturePNG(t)+`" "$2"
  shift
done
`, 0o755)
	cfg, err := NewConfig(Args{Iris: iris, Model: model, Root: filepath.Join(dir, "root")})
	if err != nil {
		t.Fatal(err)
	}
	cfg.PupDir = pup
	kit.cfg, kit.runner = cfg, NewRunner(cfg)
	return kit
}

func addPupCheckpoint(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "config.json"), "{}", 0o644)
	writeFile(t, filepath.Join(dir, "system_prompts.json"), "{}", 0o644)
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// pupSays is a Python that answers with this caption.
func pupSays(caption string, tokens int) string {
	data, _ := json.Marshal(pupAnswer{Caption: caption, Tokens: tokens})
	return "cat <<'ANSWER'\n" + string(data) + "\nANSWER\n"
}

// addRef sets the bytes of the one reference these tests use.
func (k upsampleKit) addRef(t *testing.T, content string) {
	t.Helper()
	writeFile(t, filepath.Join(k.cfg.Sessions, "s", "inputs", "ref.png"), content, 0o644)
}

func (k upsampleKit) pupRuns(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(k.runs)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "run")
}

func (k upsampleKit) render(t *testing.T, params map[string]any) *Job {
	t.Helper()
	params["session_name"] = "s"
	params["upsample"] = true
	job, err := k.runner.Submit(params)
	if err != nil {
		t.Fatal(err)
	}
	jobEnds(t, job)
	return job
}

func TestARenderIsGivenTheCaptionAndTheTakeKeepsBoth(t *testing.T) {
	kit := upsampleStudio(t, pupSays("a long caption", 300))
	job := kit.render(t, map[string]any{"prompt": "a fish", "width": 256, "height": 256})
	if state, why := job.outcome(); state != "done" {
		t.Fatalf("state %q: %s", state, why)
	}
	argv, _ := os.ReadFile(kit.argv)
	if !strings.Contains(string(argv), "-p\na long caption\n") {
		t.Errorf("iris was told:\n%s", argv)
	}
	side := readJSONObject(strings.TrimSuffix(job.outputPath(), ".png") + ".json")
	params, _ := side["params"].(map[string]any)
	if params["prompt"] != "a fish" || side["caption"] != "a long caption" || side["caption_tokens"] != 300.0 {
		t.Errorf("sidecar: prompt %v, caption %v, tokens %v", params["prompt"], side["caption"], side["caption_tokens"])
	}
	stages := job.Summary()["stages"].(stages)
	if stages[upsampleStage].State != "done" {
		t.Errorf("the stage reads %+v", stages[upsampleStage])
	}
	var req pupRequest
	data, _ := os.ReadFile(filepath.Join(kit.cfg.PupDir, "request.json"))
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	if req.Prompt != "a fish" || req.Width != 256 || !strings.HasSuffix(req.Model, "-pup") || !strings.HasSuffix(req.TextEncoder, "text_encoder") {
		t.Errorf("pup was told %+v", req)
	}
}

// The upsampler has to read which image is which, and iris does when it runs
// with none. Reuse needs the prompt as it was typed.
func TestAMentionIsResolvedForTheUpsamplerAndIrisAndTheTakeKeepsItAsTyped(t *testing.T) {
	kit := upsampleStudio(t, pupSays("a caption", 9))
	kit.addRef(t, "x")
	typed := "put @ref.png on the table"
	refs := []any{map[string]any{"name": "ref.png"}}
	job := kit.render(t, map[string]any{"prompt": typed, "refs": refs})
	request := readJSONObject(filepath.Join(kit.cfg.PupDir, "request.json"))
	if request["prompt"] != "put the first image on the table" {
		t.Errorf("the upsampler read %q", request["prompt"])
	}
	summary := job.Summary()
	if job.Params["prompt"] != typed || summary["resolved_prompt"] != "put the first image on the table" {
		t.Errorf("params %q, resolved %v", job.Params["prompt"], summary["resolved_prompt"])
	}

	plain, err := kit.runner.Submit(map[string]any{"session_name": "s", "prompt": typed, "refs": refs})
	if err != nil {
		t.Fatal(err)
	}
	jobEnds(t, plain)
	argv, _ := os.ReadFile(kit.argv)
	if !strings.Contains(string(argv), "put the first image on the table\n") {
		t.Errorf("iris read %s", argv)
	}
}

func TestAJobThatDoesNotUpsampleSkipsTheStage(t *testing.T) {
	kit := upsampleStudio(t, "exit 1\n")
	job, err := kit.runner.Submit(map[string]any{"session_name": "s", "prompt": "a fish"})
	if err != nil {
		t.Fatal(err)
	}
	if state := jobEnds(t, job); state != "done" {
		t.Fatalf("state %q", state)
	}
	if mark := job.Summary()["stages"].(stages)[upsampleStage]; mark.State != "skip" || kit.pupRuns(t) != 0 {
		t.Errorf("stage %+v after %d runs of pup", mark, kit.pupRuns(t))
	}
}

// Cancel has to reach pup itself: iris must not start, and nothing may be left
// running with 21 GB in hand.
func TestCancelStopsTheUpsamplerAndNoRenderFollows(t *testing.T) {
	kit := upsampleStudio(t, "echo $$ > pid\nexec sleep 30\n")
	job, err := kit.runner.Submit(map[string]any{"session_name": "s", "prompt": "a fish", "upsample": true})
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(kit.cfg.PupDir, "pid")
	waitUntil(t, func() bool { return FileExists(pidFile) })
	if busy := kit.runner.Busy(); busy["message"] != "Upsampling" || busy["busy"] != true {
		t.Errorf("busy said %v", busy)
	}
	if !kit.runner.Cancel(job.ID) {
		t.Fatal("the cancel was refused")
	}
	if state := jobEnds(t, job); state != "cancelled" {
		t.Errorf("state %q", state)
	}
	if FileExists(kit.argv) {
		t.Error("iris ran after the cancel")
	}
	if mark := job.Summary()["stages"].(stages)[upsampleStage]; mark.State != "cancelled" {
		t.Errorf("the stage reads %+v", mark)
	}
}

func TestAFailureSaysWhatTheUpsamplerSaid(t *testing.T) {
	kit := upsampleStudio(t, "echo 'Error: Box 0 has neither bbox' >&2\nexit 1\n")
	job := kit.render(t, map[string]any{"prompt": "a fish"})
	if state, why := job.outcome(); state != "failed" || why != "Box 0 has neither bbox" {
		t.Errorf("state %q: %s", state, why)
	}
}

func TestANewSeedReusesTheCaptionAndAnythingElseWritesAnother(t *testing.T) {
	kit := upsampleStudio(t, pupSays("a caption", 300))
	kit.addRef(t, "one")
	base := func() map[string]any {
		return map[string]any{"prompt": "a fish", "width": 256, "height": 256, "seed": 1,
			"refs": []any{map[string]any{"name": "ref.png"}}}
	}
	runs := func(want int, change func(p map[string]any)) {
		t.Helper()
		params := base()
		change(params)
		job := kit.render(t, params)
		if state, why := job.outcome(); state != "done" {
			t.Fatalf("state %q: %s", state, why)
		}
		if got := kit.pupRuns(t); got != want {
			t.Errorf("pup has run %d times, want %d", got, want)
		}
		if job.Summary()["caption"] != "a caption" {
			t.Errorf("no caption: %v", job.Summary())
		}
	}
	runs(1, func(map[string]any) {})
	runs(1, func(p map[string]any) { p["seed"] = 2 })
	runs(2, func(p map[string]any) { p["prompt"] = "a bird" })
	runs(3, func(p map[string]any) { p["width"] = 512 })
	runs(4, func(p map[string]any) { p["boxes"] = `[{"id":"a"}]` })
	kit.addRef(t, "two")
	runs(5, func(map[string]any) {})
	kit.cfg.Pup = filepath.Join(t.TempDir(), "another-pup")
	addPupCheckpoint(t, kit.cfg.Pup)
	runs(6, func(map[string]any) {})
}

func TestUpsampleOnlyWritesTheCaptionAndRendersNothing(t *testing.T) {
	kit := upsampleStudio(t, pupSays("a caption", 9000))
	server := httptest.NewServer(NewApp(kit.cfg, kit.runner))
	defer server.Close()
	code, body := post(t, server.URL, "/api/upsample", map[string]any{"session_name": "s", "prompt": "a fish", "run_mode": "oneshot"})
	if code != 200 {
		t.Fatalf("%d: %v", code, body)
	}
	job := kit.runner.jobs[anyToString(body["id"])]
	if state := jobEnds(t, job); state != "done" {
		t.Fatalf("state %q: %v", state, job.Summary())
	}
	if job.Summary()["caption"] != "a caption" || FileExists(kit.argv) || job.outputPath() != "" {
		t.Errorf("caption %v, iris ran %v, take %q", job.Summary()["caption"], FileExists(kit.argv), job.outputPath())
	}
	if saved := readJSONObject(kit.cfg.SessionSetting("s")); saved["upsample_only"] != nil {
		t.Errorf("the session kept %v", saved["upsample_only"])
	}
}

func TestACaptionOverTheLimitIsRefusedBeforeARender(t *testing.T) {
	kit := upsampleStudio(t, pupSays("a caption", maxCaptionTokens+1))
	job := kit.render(t, map[string]any{"prompt": "a fish"})
	state, why := job.outcome()
	if state != "failed" || !strings.Contains(why, "1281 tokens") || FileExists(kit.argv) {
		t.Errorf("state %q: %s; iris ran: %v", state, why, FileExists(kit.argv))
	}
	// At the limit iris reads all of it.
	exact := upsampleStudio(t, pupSays("a caption", maxCaptionTokens))
	if state := jobEnds(t, exact.render(t, map[string]any{"prompt": "a fish"})); state != "done" {
		t.Errorf("state %q at the limit", state)
	}
}

// The stub's python fails while the REPL is up, as memory does.
func TestTheReplIsUnloadedBeforeTheUpsamplerRuns(t *testing.T) {
	kit := upsampleStudio(t, `if [ -e "$REPL" ]; then exit 3; fi
`+pupSays("a caption", 300))
	repl := filepath.Join(t.TempDir(), "repl-loaded")
	t.Setenv("REPL", repl)
	iris := filepath.Join(t.TempDir(), "iris")
	writeFile(t, iris, `#!/bin/sh
touch "`+repl+`"
while IFS= read -r line; do
  case "$line" in
    "!quit") rm "`+repl+`"; exit 0 ;;
    "!"*) ;;
    *) out="`+filepath.Dir(repl)+`/image.png"; cp "`+fixturePNG(t)+`" "$out"; echo "Done -> $out (ref \$1) [1.00s]" ;;
  esac
done
rm "`+repl+`"
`, 0o755)
	kit.cfg.Iris = iris
	defer kit.runner.StopInteractive()
	if ok, msg := kit.runner.LoadInteractive(map[string]any{"session_name": "s"}); !ok {
		t.Fatal(msg)
	}
	waitUntil(t, func() bool { return FileExists(repl) })
	job := kit.render(t, map[string]any{"prompt": "a fish", "run_mode": "interactive"})
	if state, why := job.outcome(); state != "done" {
		t.Fatalf("state %q: %s", state, why)
	}
	if !kit.runner.interactiveLoaded() {
		t.Error("the render did not load the REPL again")
	}
}

// A caption is sent as it was written, on one line, whatever it starts with and
// with its table's JSON as the upsampler left it. Each iris here keeps the lines
// it was sent.
func TestACaptionSurvivesTheReplLine(t *testing.T) {
	table := `[{"id": "sign_1", "desc": "a sign reading \"TWO  SPACES\\n\"", "bbox": [1, 2, 3, 4]}]`
	cases := []struct {
		name, caption, ratio string
		refs                 []any
		want                 string
	}{
		{"a box table", "A sign <sign_1> stands. " + table, "1:1", nil, "1024x1024 A sign <sign_1> stands. " + table},
		{"a bang", "!quit is printed on the wall", "1:1", nil, "1024x1024 !quit is printed on the wall"},
		{"a dollar", "$5 bill on a table", "1:1", nil, "1024x1024 $5 bill on a table"},
		{"a line break", "A poster reading\n\"TWO\nLINES\"", "1:1", nil, `1024x1024 A poster reading "TWO LINES"`},
		{"a dollar with no size", "$5 bill on a table", "", []any{map[string]any{"name": "ref.png"}}, "$1 Image: $5 bill on a table."},
		{"a bang with no size", "!quit on a wall", "", []any{map[string]any{"name": "ref.png"}}, "$1 !quit on a wall"},
		{"a size with no size", "a wall measuring 1024x1024", "", []any{map[string]any{"name": "ref.png"}}, "$1 Image: a wall measuring 1024x1024."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kit := upsampleStudio(t, pupSays(c.caption, 300))
			lines := filepath.Join(t.TempDir(), "lines")
			iris := filepath.Join(t.TempDir(), "iris")
			writeFile(t, iris, `#!/bin/sh
n=0
while IFS= read -r line; do
  printf '%s\n' "$line" >> "`+lines+`"
  case "$line" in
    "!load "*) n=$((n+1)); echo "Loaded: x (64x64, ref \$$n)" ;;
    "!"*) ;;
    *) n=$((n+1)); cp "`+fixturePNG(t)+`" "`+lines+`.png"; echo "Done -> `+lines+`.png (ref \$$n) [1.00s]" ;;
  esac
done
`, 0o755)
			kit.cfg.Iris = iris
			defer kit.runner.StopInteractive()
			kit.addRef(t, "one")
			params := map[string]any{"prompt": "a thing", "run_mode": "interactive", "ratio": c.ratio}
			if c.refs != nil {
				params["refs"] = c.refs
			}
			job := kit.render(t, params)
			if state, why := job.outcome(); state != "done" {
				t.Fatalf("state %q: %s", state, why)
			}
			data, _ := os.ReadFile(lines)
			sent := strings.Split(strings.TrimSpace(string(data)), "\n")
			if got := sent[len(sent)-1]; got != c.want {
				t.Errorf("sent  %q\nwant  %q", got, c.want)
			}
			if job.Summary()["caption"] != c.caption {
				t.Errorf("the job kept %q", job.Summary()["caption"])
			}
		})
	}
}

func TestUpsampleIsOfferedOnlyWithAllItsParts(t *testing.T) {
	kit := upsampleStudio(t, "")
	cfg := kit.cfg
	if reason := cfg.upsampleUnavailable(); reason != "" {
		t.Fatal(reason)
	}
	check := func(want string) {
		t.Helper()
		status := cfg.upsampleStatus()
		if got := anyToString(status["upsample_reason"]); !strings.Contains(got, want) || status["upsample_available"] != (want == "") {
			t.Errorf("%v, want %q", status, want)
		}
		if want != "" && !slices.Contains(validate(cfg, map[string]any{"prompt": "a", "upsample": true}), cfg.upsampleUnavailable()) {
			t.Errorf("validate let Upsample through with %q", want)
		}
	}
	// The short folder name is found too, and so is a checkpoint chosen in Paths.
	beside := filepath.Dir(cfg.Model)
	if err := os.Rename(filepath.Join(beside, pupFolders[0]), filepath.Join(beside, pupFolders[1])); err != nil {
		t.Fatal(err)
	}
	check("")
	chosen := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Rename(filepath.Join(beside, pupFolders[1]), chosen); err != nil {
		t.Fatal(err)
	}
	check("No prompt upsampler checkpoint")
	cfg.Pup = chosen
	check("")

	python := cfg.pupPython()
	if err := os.Rename(python, python+".gone"); err != nil {
		t.Fatal(err)
	}
	check("uv sync")
	if err := os.Rename(python+".gone", python); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cfg.Model, "transformer", "config.json"), flux2Config, 0o644)
	check("FLUX.3")
}

func TestThePathsRouteTakesAnUpsamplerAndRemembersIt(t *testing.T) {
	kit := upsampleStudio(t, "")
	server := httptest.NewServer(NewApp(kit.cfg, kit.runner))
	defer server.Close()
	if code, _ := post(t, server.URL, "/api/pup", map[string]any{"pup": t.TempDir()}); code != 400 {
		t.Errorf("a folder of nothing got %d", code)
	}
	chosen := t.TempDir()
	addPupCheckpoint(t, chosen)
	code, body := post(t, server.URL, "/api/pup", map[string]any{"pup": chosen})
	if code != 200 || body["pup"] != chosen || body["upsample_available"] != true {
		t.Fatalf("%d: %v", code, body)
	}
	if saved := SavedPath(kit.cfg.Root, "pup.json", "pup"); saved != chosen {
		t.Errorf("saved %q", saved)
	}
}

func TestValidateChecksBoxesAndTheReferenceCap(t *testing.T) {
	kit := upsampleStudio(t, "")
	refs := func(n int) []any {
		list := make([]any, n)
		for i := range list {
			list[i] = map[string]any{"name": fmt.Sprintf("%d.png", i)}
		}
		return list
	}
	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"boxes off", map[string]any{"boxes": `[{"id":"a"}]`}, "need the prompt upsampler"},
		{"not JSON", map[string]any{"upsample": true, "boxes": `[{`}, "must be JSON"},
		{"not text", map[string]any{"upsample": true, "boxes": 3}, "must be JSON"},
		{"empty list", map[string]any{"upsample": true, "boxes": `[]`}, "non-empty list"},
		{"not a list", map[string]any{"upsample": true, "boxes": `{"id":"a"}`}, "non-empty list"},
		{"no id", map[string]any{"upsample": true, "boxes": `[{"desc":"a"}]`}, "non-empty list"},
		{"a number id", map[string]any{"upsample": true, "boxes": `[{"id":1}]`}, "non-empty list"},
		{"good boxes", map[string]any{"upsample": true, "boxes": `[{"id":"a"}]`}, ""},
		{"blank boxes", map[string]any{"boxes": "  "}, ""},
		{"eleven upsampled", map[string]any{"upsample": true, "refs": refs(11)}, "while Upsample is on"},
		{"ten upsampled", map[string]any{"upsample": true, "refs": refs(10)}, ""},
		{"sixteen plain", map[string]any{"refs": refs(16)}, ""},
		{"seventeen plain", map[string]any{"refs": refs(17)}, "At most 16"},
		{"a path for a reference", map[string]any{"refs": []any{map[string]any{"name": "../../secret.png"}}}, "uploaded image"},
		{"the parent for a reference", map[string]any{"refs": []any{map[string]any{"name": ".."}}}, "uploaded image"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.params["prompt"] = "a fish"
			c.params["ratio"] = "1:1"
			errs := strings.Join(validate(kit.cfg, c.params), " ")
			if (c.want == "") != (errs == "") || !strings.Contains(errs, c.want) {
				t.Errorf("got %q, want %q", errs, c.want)
			}
		})
	}
}
