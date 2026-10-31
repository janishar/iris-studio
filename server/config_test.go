package server

import "testing"

// helmstudio passes --model on every launch: it is how the launcher's checkpoint
// choice reaches the studio, so a path saved by an earlier launch must not win.
func TestALaunchUsesTheCheckpointItWasGiven(t *testing.T) {
	root := t.TempDir()
	iris := stubIris(t, fixturePNG(t))
	if _, err := NewConfig(Args{Iris: iris, Model: fakeModelDir(t), Root: root}); err != nil {
		t.Fatal(err)
	}
	chosen := fakeModelDir(t)
	cfg, err := NewConfig(Args{Iris: iris, Model: chosen, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != chosen {
		t.Errorf("model = %s, want the one this launch was given, %s", cfg.Model, chosen)
	}
}
