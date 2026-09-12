# iris studio

A local web control surface for [iris.c](https://github.com/janishar/iris.c) — native
FLUX.2 Klein / Z-Image-Turbo image generation on Apple Silicon (Metal/MPS).

iris studio is a Go stdlib-only web server (no JS build step, one small dependency —
`fsnotify`, used only for `--dev` hot reload) that drives the `iris` binary: it builds
the CLI arguments, runs one-shot or interactive (model-stays-loaded) sessions, manages
reference images and multi-reference combination, queues renders, streams live
denoising-step previews over the Kitty graphics protocol, and keeps per-session state
on disk — all from a browser tab.

## Requirements

- macOS on Apple Silicon (Metal/MPS backend; iris.c also builds a BLAS or generic
  backend on other platforms, untested here).
- Go 1.27+.
- A built `iris` binary — see below.
- At least one downloaded model directory (`flux-klein-4b`, `zimage-turbo`, etc.).

## Building the iris binary

```bash
cd iris.c
make mps        # Apple Silicon Metal GPU (fastest)
```

See [iris.c/README.md](iris.c/README.md) for the BLAS/generic backends and for
downloading model weights (`./download_model.sh 4b`, etc. — pick whichever variant
fits your disk/RAM; ~16-30GB per model).

## Running the studio

```bash
go build -o iris-studio .
./iris-studio --iris iris.c/iris --model /path/to/flux-klein-4b --port 8720
```

`--model` points directly at one downloaded model directory (the studio doesn't scan
for multiple models — you run with one loaded, matching how `iris` itself works). You
can change either path at any time from the **⚙ Paths** button in the top bar without
restarting the server.

Then open `http://127.0.0.1:8720`.

For frontend development, pass `--dev` to auto-reload the browser when files under
`static/` change.

## Features

- **Fixed model, switchable at runtime** — the model directory is set at launch via
  `--model` (labeled automatically for the known FLUX.2 Klein / Z-Image-Turbo
  variants) and shown read-only in the form; change it from **⚙ Paths** in the top
  bar, which opens the same kind of path-editing modal as `--iris`.
- **Text-to-image and image-to-image** — upload reference images and combine up to 16
  of them via iris's in-context multi-reference conditioning.
- **One-shot or interactive run mode** — one-shot spawns a fresh `iris` process per
  generation (simplest, reloads weights every time); interactive keeps a single
  `iris` REPL process loaded so repeated generations skip the multi-second-to-minute
  weight load entirely.
- **Live step preview** — with "Live step preview" enabled, each denoising step's
  intermediate frame streams into the browser via the same Kitty graphics protocol
  iris uses for terminal display.
- **Sessions** — named sessions each keep their own reference images, generated
  takes, and settings under `sessions/<name>/`, mirroring what you'd get running
  `iris` from different working directories, but switchable from the UI.
- **Terminal panel** — raw `iris` REPL output streams live; when interactive mode is
  loaded you can also type raw commands (`!explore 4 a cat`, `!help`, ...) directly.

## Session layout

```
sessions/<name>/
  inputs/         uploaded reference images
  outputs/        generated PNGs, each with a <name>.json sidecar of run metadata
  setting.json    the session's last-used prompt/params, plus a "takes" history
  terminal.log    raw iris output for this session
```

`sessions/last_session.json`, `sessions/iris.json`, and `sessions/model.json` persist
the active session and the last-used `--iris`/`--model` paths across restarts.

## A note on interactive mode

iris_cli.c's REPL (`iris_cli.c`) is designed to also work over a pipe (it falls back
to plain `fgets`-style line reading when stdin isn't a TTY), but a few of its status
lines (`Seed: ...`, `Loaded: ...`, `Done -> ...`) are `printf`'d to stdout without an
explicit flush, so they can sit in libc's block buffer indefinitely when piped rather
than connected to a real terminal. The studio works around this instead of patching
iris.c:

- Reference `$N` IDs are tracked deterministically (iris assigns them in the exact
  order `!load`/generation commands are processed), so the studio never waits on the
  `Loaded: ...` line.
- Completion is detected by polling iris's own temp output directory
  (`/tmp/iris-XXXXXX/`) for a new, size-stabilized PNG, racing against the `Done -> `
  line in case it does arrive.
- The exact realized seed (when you request a random one) isn't reliably captured in
  interactive mode for the same reason; one-shot mode captures it correctly (iris
  prints it to stderr there, which is always unbuffered).

Live preview frames and step/phase progress are unaffected — iris explicitly flushes
after each one.

## License

MIT
