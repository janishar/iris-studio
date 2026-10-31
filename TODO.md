# TODO

Open work left after adding FLUX.3 Image Klein. Delete an item when it is
merged, and this file when it is empty.

Line numbers are `server/runner.go` as of the FLUX.3 change.

## Decide first

- [ ] **Engine stdout buffering.** REPL stdout is block-buffered on a pipe, which
  delays `!help`, command acks and the last `!explore` frame, and glues `Error:`
  onto frame lines. Add `setvbuf(stdout, NULL, _IOLBF, 0);` at the top of
  `iris_cli_run` in the iris.c fork. If not, refuse `!show` in `SendInteractive`
  and stop offering `!help` in `static/index.html`. Item 2 depends on this.

## Interactive mode (every model)

One-shot is the default and is not affected by these.

- [ ] **1. Reference ids are off by one.** `nextRefID` starts at 0 and the engine
  is 1-based, so the first reference fails or the wrong image is used. Start at
  1, delete the increment at line 1112, change `loadedLnRe` to `ref \$(\d+)\)`,
  and in `readInteractive` set `nextRefID = max(nextRefID, N+1)` under `r.mu`.
- [ ] **2. A reference job can return the reference as its take.** The filesystem
  poll picks up the `!load` copy (lines 1107, 1265-1290). Delete the poll,
  `snapshotPNGs`, `discoverIrisTmpDir` and `interactiveState.tmpDir`.
- [ ] **3. Engine errors never reach the job.** A render can hang for up to an
  hour with Unload blocked. Use `(?:^|\x1b\\)Error: (.*)$` for `errorLnRe`, keep
  the text after the last ESC-backslash in `truncateKittyLine`, set awaiting
  before the `!load` loop, release `interactiveLock` before
  `awaitGeneratedImage`, and emit `interactiveBusy(false)` for Error lines and at
  reader EOF.
- [ ] **4. Panel size is ignored with references.** FLUX.3 also fails on a
  reference over 1792px. At line 1105 use
  `strings.Join(refTokens, " ") + fmt.Sprintf(" %dx%d ", w, h) + prompt`.
- [ ] **5. Load model leaves Generate disabled.** Delete the `Loading ` busy
  branch at lines 963-965.
- [ ] **6. Unload then Generate fails at once.** At lines 926-929 replace the
  select with `if r.awaitingImage() { r.interactiveLines <- nil }`.
- [ ] **7. Cancel does nothing on an interactive job.** In `Runner.Cancel`'s
  running case, when proc is nil, unlock, `go r.StopInteractive()`, return true.
- [ ] **8. `!explore` looks idle.** With the engine change, add a busy pattern for
  `^Generating \d+ images` and clear it on `^Done\. Use`. Without it, only fix the
  comment at lines 1145-1146. Explore images stay throwaway previews.
- [ ] **9. Tests and docs.** Give the stub in `server/interactive_test.go` a frame
  without a newline and a 1-based `!load`, add a test that a reference job's
  output is not the reference, and rewrite the README's interactive note and the
  Limits lines on Unload and on there being no interactive test.

## One-shot (every model)

- [ ] **Random seeds are recorded wrongly.** With seed -1 the engine prints one
  seed and uses a later clock reading, so the take cannot be reproduced. In
  `buildOneshotArgs`, resolve `seed < 0` to `time.Now().UnixMilli()` and always
  send `-S`. Add one assert and reword the README's seed note.
- [ ] **Failed renders show only the exit code.** In `handleLine`, remember the
  last `errorLnRe` match on the Job and use it at line 667, falling back to the
  exit code.

## FLUX.3

- [ ] **Test `flux_3_klein_base`.** Declared but never run: this Hugging Face
  account has no access yet and iris.c calls it untested. Accept the licence on
  the base repo, run one render, then remove "Not yet exercised" from the README.
- [ ] **Update the helmstudio registry.** Once this is on `main`, make the same
  manifest changes in helmstudio's `studios/iris-studio.yaml` and its README's
  iris row. Until then FLUX.3 cannot be picked at install on a machine that has
  not fetched iris studio. Also correct the header comment in `helmstudio.yaml`,
  which says the registry needs no copy.

## Not tested on the new engine

- [ ] **FLUX.2 and Z-Image.** Neither has run since the engine merge, because no
  checkpoint is on this machine. Run `iris.c/run_test.py` with a `flux-klein-4b`
  checkpoint, since FLUX.2's transformer changed a lot.
- [ ] **`peak_ram_gb`.** 35 is measured on FLUX.3 only. Measure the others.

## Local cleanup

- [ ] `.vscode/tasks.json` points at `../flux-klein-9b`, which is not on this
  machine.
- [ ] Remove the `flux3-check` test session and its 3 takes, also in the
  helmstudio gallery.
