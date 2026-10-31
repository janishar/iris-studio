package server

import (
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	flux2Config  = `{"_class_name": "FluxTransformer2DModel"}`
	flux3Config  = `{"_class_name": "Flux3Transformer2DModel"}`
	zimageConfig = `{"cap_feat_dim": 2560}`
)

// planConfig is a studio on a checkpoint with this transformer config. It sits
// in a folder that says nothing of its family, as iris reads it.
func planConfig(t *testing.T, transformer string) *Config {
	t.Helper()
	model := fakeModelDir(t)
	if err := os.WriteFile(filepath.Join(model, "transformer", "config.json"), []byte(transformer), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Config{Model: model, Sessions: t.TempDir()}
}

// addImage puts a real image of this size among the session's references.
func addImage(t *testing.T, cfg *Config, name string, w, h int) {
	t.Helper()
	inputs, _, err := cfg.SessionDirs("refs")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(inputs, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img := image.NewGray(image.Rect(0, 0, w, h))
	if strings.HasSuffix(name, ".jpg") {
		err = jpeg.Encode(f, img, nil)
	} else {
		err = png.Encode(f, img)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func refsOf(names ...string) []any {
	list := []any{}
	for _, name := range names {
		list = append(list, map[string]any{"name": name})
	}
	return list
}

func TestThePlanStatesTheSizeARenderWillComeOut(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		want   sizePlan
	}{
		{"auto with nothing is the default, sent because iris's own is 256",
			map[string]any{}, sizePlan{1024, 1024, "default 1:1", true}},
		{"auto with a typed side keeps the other from the default",
			map[string]any{"height": 512}, sizePlan{1024, 512, "default 1:1", true}},
		{"a ratio is sent", map[string]any{"ratio": "16:9"}, sizePlan{1360, 768, "chosen", true}},
		{"a typed side overrides that side of a ratio",
			map[string]any{"ratio": "3:2", "width": 512}, sizePlan{512, 832, "chosen", true}},
		{"a ratio outranks a reference",
			map[string]any{"ratio": "9:16", "refs": refsOf("wide.png")}, sizePlan{768, 1360, "chosen", true}},
		{"both sides outrank a ratio",
			map[string]any{"ratio": "1:1", "width": 512, "height": 256}, sizePlan{512, 256, "width × height", true}},
		{"an old session with numbers and no ratio key renders at those numbers",
			map[string]any{"width": 832, "height": 1216, "refs": refsOf("wide.png")}, sizePlan{832, 1216, "width × height", true}},
		{"a saved session's numbers arrive as JSON numbers too",
			map[string]any{"width": json.Number("832"), "height": float64(1216)}, sizePlan{832, 1216, "width × height", true}},
		{"one reference stands at exactly its own size, off the grid",
			map[string]any{"refs": refsOf("wide.png")}, sizePlan{1001, 667, "follows the first image", false}},
		{"a JPEG reference measures too",
			map[string]any{"refs": refsOf("photo.jpg")}, sizePlan{200, 120, "follows the first image", false}},
		{"several references are sized by the first, cropped to the grid",
			map[string]any{"refs": refsOf("wide.png", "photo.jpg")}, sizePlan{992, 656, "follows the first image", false}},
		{"a typed side turns the crop off, so the other side is the canvas's and both are sent",
			map[string]any{"width": 512, "refs": refsOf("wide.png")}, sizePlan{512, 672, "follows the first image", true}},
		{"a typed side over several references is on the grid already",
			map[string]any{"height": 512, "refs": refsOf("wide.png", "photo.jpg")}, sizePlan{992, 512, "follows the first image", true}},
		{"a reference that cannot be read fails nothing and claims no size",
			map[string]any{"refs": refsOf("anim.webp")}, sizePlan{0, 0, "follows the first image", false}},
		{"a reference that is missing does the same",
			map[string]any{"refs": refsOf("gone.png")}, sizePlan{0, 0, "follows the first image", false}},
		{"a typed side stands, unsent, when the other cannot be worked out",
			map[string]any{"width": 512, "refs": refsOf("anim.webp")}, sizePlan{512, 0, "follows the first image", false}},
	}
	cfg := planConfig(t, flux3Config)
	addImage(t, cfg, "wide.png", 1001, 667)
	addImage(t, cfg, "photo.jpg", 200, 120)
	addRef(t, cfg, "anim.webp", "RIFF....WEBP")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.params["session_name"] = "refs"
			if got := planSize(cfg, c.params); got != c.want {
				t.Errorf("planned %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestThePlanReadsTheFamilyFromTheCheckpoint(t *testing.T) {
	params := map[string]any{"session_name": "refs", "refs": refsOf("wide.png")}
	for name, c := range map[string]struct {
		transformer string
		want        sizePlan
	}{
		"FLUX.2 takes the reference floored to the grid":   {flux2Config, sizePlan{992, 656, "follows the first image", false}},
		"Z-Image takes no reference, so it states no size": {zimageConfig, sizePlan{0, 0, "follows the first image", false}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := planConfig(t, c.transformer)
			addImage(t, cfg, "wide.png", 1001, 667)
			if got := planSize(cfg, params); got != c.want {
				t.Errorf("planned %+v, want %+v", got, c.want)
			}
		})
	}
}

// The expected sizes come from the sizing lines of iris.c, run as C.
func TestAReferenceComesOutAtTheEnginesSize(t *testing.T) {
	cases := []struct {
		family       string
		w, h, count  int
		typed        bool
		wantW, wantH int
	}{
		{"flux3", 1001, 667, 1, false, 1001, 667},
		{"flux3", 1001, 667, 1, true, 1008, 672},
		{"flux3", 40, 24, 1, false, 40, 24},
		{"flux3", 40, 24, 1, true, 64, 64},
		{"flux3", 4000, 3000, 1, false, 1792, 1344},
		{"flux3", 6000, 1000, 1, false, 1792, 299},
		{"flux3", 6000, 1000, 1, true, 1792, 304},
		{"flux3", 1001, 667, 3, false, 992, 656},
		{"flux3", 1200, 1000, 2, false, 1120, 928},
		{"flux3", 4000, 3000, 2, false, 1168, 880},
		{"flux3", 6000, 1000, 2, false, 1792, 288},
		{"flux3", 40, 24, 2, false, 64, 64},
		{"flux2", 1001, 667, 1, false, 992, 656},
		{"flux2", 6000, 1000, 1, false, 1776, 288},
		{"flux2", 100, 40, 1, false, 96, 32},
		{"flux2", 100, 40, 1, true, 96, 64},
		{"flux2", 100, 10, 1, true, 96, 64},
	}
	for _, c := range cases {
		got := referenceSize(c.family, c.w, c.h, c.count, c.typed)
		if got != [2]int{c.wantW, c.wantH} {
			t.Errorf("%s %dx%d of %d, typed %v: %v, want %dx%d", c.family, c.w, c.h, c.count, c.typed, got, c.wantW, c.wantH)
		}
	}
}

// With no size iris sizes the render itself, and only with neither flag: main.c
// reads a lone -W as no size at all.
func TestOneShotSendsBothSidesOrNeither(t *testing.T) {
	cfg := planConfig(t, flux3Config)
	addImage(t, cfg, "wide.png", 1001, 667)
	sized := func(extra map[string]any) []string {
		params := map[string]any{"session_name": "refs", "prompt": "a cat", "refs": refsOf("wide.png")}
		for k, v := range extra {
			params[k] = v
		}
		args := buildOneshotArgs(cfg, params, "in", "out.png", false)
		i, j := slices.Index(args, "-W"), slices.Index(args, "-H")
		if i < 0 || j < 0 {
			if i != j {
				t.Fatalf("one side of the size was sent: %q", args)
			}
			return nil
		}
		return []string{args[i+1], args[j+1]}
	}
	if got := sized(nil); got != nil {
		t.Errorf("auto with a reference sent a size: %v", got)
	}
	if got := sized(map[string]any{"width": 512}); !slices.Equal(got, []string{"512", "672"}) {
		t.Errorf("a typed side sent %v", got)
	}
	if got := sized(map[string]any{"refs": []any{}}); !slices.Equal(got, []string{"1024", "1024"}) {
		t.Errorf("auto with no reference sent %v", got)
	}
}

// !size would hold, and a size on the line would turn the crop-back off.
func TestTheReplLeavesAnAutoRenderToTheReference(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	addRef(t, cfg, "ref.png", "the reference")

	params := interactiveParams("a cat", "ref.png")
	delete(params, "width")
	delete(params, "height")
	job, err := runner.Submit(params)
	if err != nil {
		t.Fatal(err)
	}
	if state := jobEnds(t, job); state != "done" {
		t.Fatalf("state %q: %v", state, job.Summary())
	}
	command := job.Summary()["command"].([]string)
	if got := command[len(command)-1]; got != "$1 a cat" {
		t.Errorf("the line was %q", got)
	}
	if strings.Contains(strings.Join(command, "\n"), "!size") {
		t.Errorf("a size was sent: %q", command)
	}
}

func TestValidationReadsTheSizeTheWayThePlanDoes(t *testing.T) {
	cfg := planConfig(t, flux3Config)
	addImage(t, cfg, "wide.png", 1001, 667)
	addRef(t, cfg, "anim.webp", "RIFF....WEBP")
	base := func(extra map[string]any) map[string]any {
		params := map[string]any{"session_name": "refs", "prompt": "a cat", "refs": []any{}}
		for k, v := range extra {
			params[k] = v
		}
		return params
	}
	for name, extra := range map[string]map[string]any{
		"auto":                      {},
		"a ratio":                   {"ratio": "4:3"},
		"a zero side":               {"width": 0, "height": 512},
		"the smallest":              {"width": 64, "height": 64},
		"the largest":               {"width": 1792, "height": 1792},
		"both sides over webp":      {"width": 512, "height": 512, "refs": refsOf("anim.webp")},
		"one side over a png":       {"width": 512, "refs": refsOf("wide.png")},
		"$ with a ratio":            {"prompt": "$9 million", "run_mode": "interactive", "ratio": "1:1", "refs": refsOf("wide.png")},
		"$ with typed sides":        {"prompt": "$9 million", "run_mode": "interactive", "width": 256, "height": 256, "refs": refsOf("wide.png")},
		"$ with no reference":       {"prompt": "$9 million", "run_mode": "interactive"},
		"$ through a one-shot":      {"prompt": "$9 million", "run_mode": "oneshot", "refs": refsOf("wide.png")},
		"a $ later in a prompt":     {"prompt": "it cost $9", "run_mode": "interactive", "refs": refsOf("wide.png")},
		"a size with a ratio":       {"prompt": "1024x1024", "run_mode": "interactive", "ratio": "1:1", "refs": refsOf("wide.png")},
		"a size through a one-shot": {"prompt": "a cat 1024x1024", "run_mode": "oneshot", "refs": refsOf("wide.png")},
		"a size mid-prompt":         {"prompt": "a 512x512 cat", "run_mode": "interactive", "refs": refsOf("wide.png")},
		"a size iris refuses":       {"prompt": "16x9 film still 8000x8000", "run_mode": "interactive", "refs": refsOf("wide.png")},
		"a size stuck to a word":    {"prompt": "cat512x512", "run_mode": "interactive", "refs": refsOf("wide.png")},
	} {
		if errs := validate(cfg, base(extra)); len(errs) != 0 {
			t.Errorf("%s was refused: %q", name, errs)
		}
	}
	for name, c := range map[string]struct {
		extra map[string]any
		want  string
	}{
		"under the floor":        {map[string]any{"width": 32}, "at least 64"},
		"negative":               {map[string]any{"height": -16}, "at least 64"},
		"over the cap":           {map[string]any{"height": 1808}, "at most 1792"},
		"off the grid":           {map[string]any{"width": 1000}, "multiples of 16"},
		"an unknown ratio":       {map[string]any{"ratio": "5:4"}, "aspect ratios"},
		"a side with no partner": {map[string]any{"width": 512, "refs": refsOf("anim.webp")}, "both width and height"},
		"$ with no size in front": {map[string]any{"prompt": "$9 million", "run_mode": "interactive", "refs": refsOf("wide.png")},
			"starting with $"},
		"$ behind a space, which the line drops": {map[string]any{"prompt": "  $9 million", "run_mode": "interactive", "refs": refsOf("wide.png")},
			"starting with $"},
		"a prompt that is only a size": {map[string]any{"prompt": "1024x1024", "run_mode": "interactive", "refs": refsOf("wide.png")},
			"start or end"},
		"a prompt ending in a size": {map[string]any{"prompt": "a cat 1024x1024", "run_mode": "interactive", "refs": refsOf("wide.png")},
			"start or end"},
		"a prompt starting with a size": {map[string]any{"prompt": "512x768 a cat", "run_mode": "interactive", "refs": refsOf("wide.png")},
			"start or end"},
		"a size at the start running into a word": {map[string]any{"prompt": "64X1792cats", "run_mode": "interactive", "refs": refsOf("wide.png")},
			"start or end"},
		"a good size at the end behind a bad one": {map[string]any{"prompt": "16x9 a cat 512x512", "run_mode": "interactive", "refs": refsOf("wide.png")},
			"start or end"},
	} {
		errs := validate(cfg, base(c.extra))
		if !strings.Contains(strings.Join(errs, " "), c.want) {
			t.Errorf("%s: %q does not say %q", name, errs, c.want)
		}
	}
}

// Auto leaves params with no size, and they must stay as asked, because Reuse
// reads the typed sides back from them. What came out sits beside them.
func TestATakeRecordsItsModelAndTheSizeItCameOut(t *testing.T) {
	cfg, err := NewConfig(Args{Iris: stubIris(t, fixturePNG(t)), Model: fakeModelDir(t), Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(cfg)
	job, err := runner.Submit(map[string]any{"session_name": "s", "prompt": "a", "run_mode": "oneshot", "ratio": "16:9"})
	if err != nil {
		t.Fatal(err)
	}
	if state := jobEnds(t, job); state != "done" {
		t.Fatalf("state %q: %v", state, job.Summary())
	}
	waitUntil(t, func() bool { return len(listImageDir(filepath.Join(cfg.Sessions, "s", "outputs"), 0)) == 1 })
	tile := listImageDir(filepath.Join(cfg.Sessions, "s", "outputs"), 0)[0]["meta"].(map[string]any)
	// The stub's take is the one-pixel fixture, whatever it was asked for.
	if tile["width"] != float64(1) || tile["height"] != float64(1) {
		t.Errorf("the sidecar has %vx%v", tile["width"], tile["height"])
	}
	if want, _ := labelFor(filepath.Base(cfg.Model)); tile["model"] != want {
		t.Errorf("the sidecar says the model is %v", tile["model"])
	}
	summary := job.Summary()
	if summary["width"] != int64(1) || summary["model"] != tile["model"] {
		t.Errorf("the job says %v, %v", summary["width"], summary["model"])
	}
	if params := summary["params"].(map[string]any); params["width"] != nil || params["ratio"] != "16:9" {
		t.Errorf("the params were not left as asked: %v", params)
	}
	if _, ok := job.Params["width"]; ok {
		t.Error("the job's params were written after newJob")
	}
}

func TestThePageCanAskForThePlan(t *testing.T) {
	cfg, runner := interactiveStudio(t)
	defer runner.StopInteractive()
	srv := httptest.NewServer(NewApp(cfg, runner))
	defer srv.Close()
	addRef(t, cfg, "anim.webp", "RIFF....WEBP")
	addImage(t, cfg, "small.png", 100, 60)

	// The studio's stub checkpoint has no transformer config, which iris reads
	// as FLUX.2.
	code, answer := post(t, srv.URL, "/api/plan", map[string]any{"session_name": "refs", "refs": refsOf("small.png")})
	if code != 200 || answer["width"] != float64(96) || answer["height"] != float64(48) ||
		answer["source"] != "follows the first image" || answer["sent"] != false {
		t.Errorf("%d %v", code, answer)
	}
	code, answer = post(t, srv.URL, "/api/plan", map[string]any{"session_name": "refs", "refs": refsOf("anim.webp")})
	if code != 200 || answer["width"] != float64(0) || answer["source"] != "follows the first image" {
		t.Errorf("%d %v", code, answer)
	}
	code, answer = post(t, srv.URL, "/api/plan", map[string]any{"ratio": "4:3", "width": 512})
	if code != 200 || answer["width"] != float64(512) || answer["height"] != float64(880) || answer["sent"] != true {
		t.Errorf("%d %v", code, answer)
	}
}

func TestANewSessionStartsOnAuto(t *testing.T) {
	settings := defaultSessionSettings("new")
	if settings["ratio"] != "" || settings["width"] != 0 || settings["height"] != 0 {
		t.Errorf("%v", settings)
	}
}
