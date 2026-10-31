# TODO

Open work left after adding FLUX.3 Image Klein. Delete an item when it is
merged, and this file when it is empty.

## FLUX.3

- [ ] **Test `flux_3_klein_base`.** Declared but never run: this Hugging Face
  account has no access yet and iris.c calls it untested. Accept the licence on
  the base repo, run one render, then remove "Not yet exercised" from the README.
- [ ] **Long prompts crash iris on Metal.** FLUX.3 segfaults (exit 139) once a
  prompt's padded length passes 512, about 480 tokens, well under the 1280
  limit: 464 tokens render and 745 crash. The GPU path in `qwen3_forward` stops
  at 512 (`iris_qwen3.c`), and the crash is a NULL dereference under
  `qwen3_attention_forward`. Fix it in iris.c, or refuse the prompt first. Fix
  this before the upsampler: a two-box caption already reached 458 tokens.

## Not tested on the new engine

- [ ] **FLUX.2 and Z-Image.** Neither has run since the engine merge, because no
  checkpoint is on this machine. Run `iris.c/run_test.py` with a `flux-klein-4b`
  checkpoint, since FLUX.2's transformer changed a lot.
- [ ] **`peak_ram_gb`.** 35 is measured on FLUX.3 only. Measure the others.

## Local cleanup

- [ ] `.vscode/tasks.json` points at `../flux-klein-9b`, which is not on this
  machine.
- [ ] Remove the `flux3-check` test session and its takes, also in the
  helmstudio gallery.
