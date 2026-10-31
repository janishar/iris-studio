package server

import (
	"path/filepath"
	"strings"
)

// knownLabels maps the directory names produced by download_model.sh/py to a
// friendly label. Anything else falls back to a guess from the folder name.
// FLUX.3 has two names each: helmstudio keeps a checkpoint under its repo's
// name and download_model.sh shortens it. The label carries the licence
// because a one-shot render passes --no-license-info, which hides iris's own
// notice.
var knownLabels = map[string]string{
	"flux-klein-4b":           "FLUX.2 Klein 4B (distilled)",
	"flux-klein-4b-base":      "FLUX.2 Klein 4B (base)",
	"flux-klein-9b":           "FLUX.2 Klein 9B (distilled, non-commercial)",
	"flux-klein-9b-base":      "FLUX.2 Klein 9B (base, non-commercial)",
	"flux-3-klein":            "FLUX.3 Image Klein (distilled, Kommunity licence)",
	"flux-3-klein-base":       "FLUX.3 Image Klein (base, Kommunity licence)",
	"flux-3-image-klein":      "FLUX.3 Image Klein (distilled, Kommunity licence)",
	"flux-3-image-klein-base": "FLUX.3 Image Klein (base, Kommunity licence)",
	"zimage-turbo":            "Z-Image-Turbo 6B",
}

// isModelDir reports whether path looks like an iris model directory, per
// the layout download_model.sh/download_model.py produce: top-level
// transformer/ and vae/ subdirectories holding the weights. A bare
// model_index.json isn't enough on its own to tell an iris (Flux/Z-Image)
// model apart from an h3.c (MiniMax-H3) one, since both use the same
// diffusers convention — h3.c's model just nests transformer/vae under
// per-modality subdirectories (FL2VA/, Ref2VA/, ...) instead of at the root.
func isModelDir(path string) bool {
	return DirExists(filepath.Join(path, "transformer")) && DirExists(filepath.Join(path, "vae"))
}

// labelFor guesses a friendly display label and model family ("flux" |
// "zimage" | "unknown") from a model directory's basename.
func labelFor(name string) (string, string) {
	if label, ok := knownLabels[name]; ok {
		family := "flux"
		if strings.Contains(name, "zimage") {
			family = "zimage"
		}
		return label, family
	}
	lower := strings.ToLower(name)
	family := "unknown"
	switch {
	case strings.Contains(lower, "zimage") || strings.Contains(lower, "z-image"):
		family = "zimage"
	case strings.Contains(lower, "flux"):
		family = "flux"
	}
	return name, family
}
