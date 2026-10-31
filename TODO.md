# TODO

Open work left after adding FLUX.3 Image Klein. Delete an item when it is
merged, and this file when it is empty.

## FLUX.3

- [ ] **Test `flux_3_klein_base`.** Declared but never run: this Hugging Face
  account has no access yet and iris.c calls it untested. Accept the licence on
  the base repo, run one render, then remove "Not yet exercised" from the README.

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
