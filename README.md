# iris studio

**A local web studio for [iris.c](https://github.com/janishar/iris.c)** — native
FLUX.2 Klein and Z-Image image generation on Apple Silicon.

iris studio is a Go server (no JS build step, two Go dependencies) that drives
the `iris` binary: it builds the CLI arguments, runs one-shot or interactive
sessions, manages reference images, queues renders and streams live denoising
previews — in a browser tab, nothing sent off your machine. It runs as a
[helmstudio][helmstudio] studio and only that way: helmstudio installs it,
launches it, gives it the directory it keeps sessions in, and takes every
finished take into the library it shares with the other studios.

[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![Platform](https://img.shields.io/badge/platform-macOS%20%28Apple%20Silicon%29-lightgrey?logo=apple)](#requirements)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

## Motivation

Image diffusion on Apple Silicon usually means PyTorch's `mps` backend or a
node-graph app around it — slow for this workload, and holding far more unified
memory than the model needs. iris.c is a native Metal implementation of FLUX.2
Klein and Z-Image with no PyTorch or MLX in the loop; iris studio makes it a
tool you can use, without a Python stack.

## Table of contents

- [Requirements](#requirements) · [Installation](#installation) —
  [Path A: the launcher](#path-a-install-from-the-launcher-recommended) ·
  [Path B: a checkout](#path-b-run-from-a-checkout) ·
  [`scripts/run.sh`](#what-scriptsrunsh-does) ·
  [Troubleshooting](#troubleshooting) · [Debugging](#debugging-and-vs-code)
- [Using the studio](#using-the-studio) — [Flags](#command-line-reference) ·
  [What helmstudio adds](#what-helmstudio-adds)
- [Features](#features) · [Sessions and state](#sessions-and-state) ·
  [Interactive mode](#a-note-on-interactive-mode)
- [Security](#security) · [Limits](#limits) ·
  [Contributing](#contributing) · [License](#license)

## Requirements

### Hardware and OS

macOS on Apple Silicon. The engine's Metal/MPS backend is what this is for;
iris.c also builds BLAS and generic backends for other platforms, untested
here. A checkpoint wants 16–30 GB of disk, and `mmap` is on by default, so
inference is often possible with less RAM than the model's size.

### Toolchain

| | Why |
| --- | --- |
| **helmstudio's `helm`** | What runs iris studio. It does not start without it — see [Installation](#installation). |
| `git`, `make`, `cc`, `xxd` | Building the engine, `make mps` in the `iris.c` submodule. |
| Go 1.27+ | Building the server. The first build fetches helmstudio's runtime SDK through the Go module proxy, so it wants the network once. |

### Checkpoints

One at a time: `--model` points at a single checkpoint directory, which is how
`iris` itself works. helmstudio downloads the one you choose at install and can
fetch the others later; a checkout links one you already have. What
`iris.c/download_model.sh` offers:

| Name | Steps | Disk | Notes |
| --- | --- | --- | --- |
| `4b` | 4 | ~16 GB | Distilled. The one to start with. |
| `4b-base` | 50 | ~16 GB | Base, CFG. |
| `9b` | 4 | ~30 GB | Distilled, **non-commercial** license, gated. |
| `9b-base` | 50 | ~30 GB | Base, CFG, **non-commercial**, gated. |
| `zimage-turbo` | 9 | ~22 GB | Z-Image-Turbo 6B, Apache 2.0. |

The two `9b` variants are gated on Hugging Face: accept the licence there, then
pass `--token`.

## Installation

Two ways in — **iris studio never starts on its own**, since whatever launches
it also gives it a session directory and the library it records takes into.

| | **A — Install it** | **B — Run from a checkout** |
| --- | --- | --- |
| For | using the studio | changing the studio |
| Needs | the **launcher**, helmstudio's daemon and web UI | the **`helm` CLI**; `helm dev` runs one studio, no daemon |
| Install | one click in its library | `git clone`, `make mps`, `bash scripts/run.sh` |
| Checkpoints | it downloads or links them | you point `IRIS_MODEL` at one |

### Path A: Install from the launcher (recommended)

The launcher clones this repository, runs the build steps in
[`helmstudio.yaml`](helmstudio.yaml) and fetches the checkpoint you pick.

**1. Install the launcher** — helmstudio itself, a daemon and a web UI. Either
**the Mac app** from [its releases][helm-releases], which bundles the daemon and
is **unsigned**, so macOS calls it damaged until you clear the quarantine flag
once (the release notes give the line); or a clone:

```bash
git clone https://github.com/janishar/helmstudio && cd helmstudio && make build && ./bin/helmstudio
```

Then open **http://127.0.0.1:8700**; everything lives in `~/.helmstudio`. Not
the `helm` installer — that is the studio author's CLI, with no library and no
gallery, and is what [Path B](#path-b-run-from-a-checkout) wants.

**2. Install iris studio from its library.** It is in helmstudio's registry, so
it is already listed. Install it and it first shows every command it will run
and every weight it could fetch: the engine (`make mps` in the submodule), the
server (`go build`), and which of the five checkpoints to launch with. The
other four can be fetched later.

**3. Start it.** The launcher gives the studio a port, the checkpoint's path and
a data directory of its own, then opens its page →
[Using the studio](#using-the-studio).

[helmstudio]: https://github.com/janishar/helmstudio
[helm-releases]: https://github.com/janishar/helmstudio/releases

### Path B: Run from a checkout

The developer's path: a checkout run against helmstudio's platform API, with
nothing installed into helmstudio. [`scripts/run.sh`](scripts/run.sh) builds the
server, links your checkpoint and starts everything under `helm dev`, so the
engine is the only thing you build by hand.

```bash
# 1. helm, the CLI that runs this checkout
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/janishar/helmstudio/main/installer/install.sh)"

# 2. the checkout, with iris.c as a submodule
git clone --recurse-submodules https://github.com/janishar/iris-studio && cd iris-studio

# 3. the engine → iris.c/iris, the one thing the script does not build
make mps -C iris.c

# 4. a checkpoint, if you have none — see Checkpoints above
(cd iris.c && ./download_model.sh 4b)

# 5. start, or restart; then `stop` to end it
IRIS_MODEL=$PWD/iris.c/flux-klein-4b IRIS_WEIGHT=flux_klein_4b bash scripts/run.sh
bash scripts/run.sh stop
```

`scripts/run.sh` needs a `helm` with `dev -select`, which is newer than
1.0.0-rc.3. All five checkpoints are `selectable`, so `{models.selected}` has to
be told which one to be; an install asks on its approval screen, and a checkout
has no approval screen. The script checks for the flag and says so rather than
failing on an unknown one.

### What `scripts/run.sh` does

1. **Checks** that `helm` is on `PATH`, that it has `dev -select`, and that
   `iris.c/iris` is built — naming the fix for each.
2. **Builds `./iris-studio`,** every run, because `helm dev` runs no build steps
   of its own and would otherwise launch whatever was there.
3. **Links the checkpoint** as `IRIS_WEIGHT` when `IRIS_MODEL` is set, per file:
   nothing is downloaded and the directory is never written to.
4. **Selects that checkpoint,** which is what `{models.selected}` resolves to.
5. **Runs `helm dev -f helmstudio.yaml`,** which supplies the platform, a data
   directory and the `/helm/` proxy. Extra arguments go to `helm dev`.

| Variable | Default | What it does |
| --- | --- | --- |
| `IRIS_MODEL` | *(none)* | Checkpoint directory to link. Needed on the first run only — `helm dev` records where the weight was linked. |
| `IRIS_WEIGHT` | `flux_klein_9b` | Which of the five declared checkpoints that is, and which one to launch with. |
| `HELM` | `helm` | A particular `helm` instead of the one on `PATH`. |
| `IRIS_DLV` | *(none)* | Port to listen for a debugger on — see [below](#debugging-and-vs-code). |

Sessions and everything else the studio keeps go to `.helm/`, the pid file and
the debugger's shim to `.cache/iris-studio`; both are gitignored, and nothing is
written into the checkout. The manifest's command carries no `--dev`, so a
front-end edit means re-running the script, not refreshing the browser.

### Troubleshooting

| What you see | What it means |
| --- | --- |
| `helm is not installed.` | Run the installer in [Path B](#path-b-run-from-a-checkout), or set `HELM`. |
| `this helm has no 'dev -select'` | The `helm` on `PATH` predates the flag. Build one from a helmstudio checkout and point `HELM` at it. |
| `iris.c/iris is not built` | `git submodule update --init --recursive`, then `make mps -C iris.c`. |
| `weights: no checkpoint at …` | `IRIS_MODEL` must name a directory holding `transformer/` and `vae/`. |
| `no checkpoint is chosen for this studio` | `IRIS_WEIGHT` names a weight the manifest does not declare. |
| `iris studio runs under helmstudio.` | The binary was started by hand. Use `bash scripts/run.sh`, or the launcher. |
| `/helm/` assets 404 | A stale `./iris-studio` — re-run the script. |
| `dlv is not installed` | `go install github.com/go-delve/delve/cmd/dlv@latest` |

### Debugging, and VS Code

```bash
IRIS_MODEL=/path/to/flux-klein-4b IRIS_WEIGHT=flux_klein_4b IRIS_DLV=2345 bash scripts/run.sh
```

`helm dev` hands the studio a restricted environment and the manifest names
`./iris-studio`, not a debugger — so the binary moves aside and that name
becomes a shim running it under [Delve][dlv] on the port, built `-N -l` so
stepping follows the source and given `--continue` so the studio starts instead
of waiting for a client. A run without `IRIS_DLV` builds over the shim.

VS Code has one launch configuration, because there is one way to run the
studio: **iris studio** (`F5`) runs **debug: iris studio** — the script with
`IRIS_DLV=2345` — and attaches the Go debugger; ending it runs **stop: iris
studio**. `.vscode/tasks.json` holds those and four more (**run: iris studio**
without the debugger, **build: iris studio**, **build: iris binary (mps)**,
**test: go (race)**) and the `IRIS_MODEL` each run uses.

[dlv]: https://github.com/go-delve/delve

## Using the studio

Write a prompt, pick a size, press **Generate**. Choose **One-shot** or
**Interactive (fast)** in the left pane: interactive `iris` starts on **Load
model** or the first interactive render, so starting the studio never loads the
model. What each panel does is [Features](#features); where the work is kept is
[Sessions and state](#sessions-and-state).

### Command-line reference

You never type this — the launcher and `scripts/run.sh` both build it from
`helmstudio.yaml`'s `processes[0].cmd` — but the flags are worth knowing:

```bash
./iris-studio --iris ./iris.c/iris --model <checkpoint> --port <port> --root <data>
```

The paths are remembered in `<data>/sessions/iris.json` and `model.json`, so a
later launch can omit them; a flag or environment variable always wins.
**There is no authentication** — see [Security](#security) before binding to
anything but `127.0.0.1`.

| Flag | Default | Description |
| --- | --- | --- |
| `--root` | *(required)* | Directory holding `sessions/` — the data directory helmstudio gives this studio. It creates none of its own and will not start without one, and a take has to be inside it for helmstudio to adopt it. |
| `--iris` | `$IRISSTUDIO_IRIS`, else last used | Path to the built `iris` binary. |
| `--model` | `$IRISSTUDIO_MODEL`, else last used | Path to one checkpoint directory. |
| `--host` | `127.0.0.1` | Bind address. |
| `--port` | `8720` | Bind port; helmstudio passes the one it allocated. |
| `--dev` | `false` | Reload the browser when files under `static/` change. `static/` is looked for beside the binary, one level up, then in the working directory — never under `--root`, which holds no source. |

Both paths are also changeable from **⚙ Paths** in the top bar while the studio
runs, which is how you switch checkpoints without a relaunch.

### What helmstudio adds

iris studio draws its own page — form, preview, takes rail, terminal — and
helmstudio adds four things around it:

| | |
| --- | --- |
| **Gallery** | helmstudio's own grid over this studio's takes, live: a take that finishes appears without a reload. Scoped to this studio; its library is where these sit beside other studios' work. |
| **References from anywhere** | **from gallery**, beside Reference images, picks an image out of that grid and copies it into this session's `inputs/`. It can be another studio's image — that is what a shared gallery is for. |
| **Render log** | A second Terminal tab streaming the render as helmstudio sees it; it reconnects after a dropped stream and says what it missed. Output's input line and **clear** hide while it shows — helm-terminal brings its own. |
| **The launcher** | A render appears there as a job with its progress and its log, so what this studio is doing is visible from outside it. |

All of it arrives through the same-origin `/helm/` proxy the server mounts, as
do the theme and this studio's colour, so the page holds no token of
helmstudio's. If those components cannot load the page still works: the three
controls stay hidden and everything else is untouched.

There is no timeline. A helmstudio sequence is an edit of video clips, and this
studio makes stills.

## Features

- **One checkpoint, switchable while it runs** — set at launch by `--model` and
  labelled automatically for the known FLUX.2 Klein and Z-Image variants;
  change it from **⚙ Paths** in the top bar, which opens the same kind of
  path-editing modal as `--iris`.
- **Text-to-image and image-to-image** — upload reference images and combine up
  to 16 of them through iris's in-context multi-reference conditioning.
  References are ordered, and the model reads that order.
- **One-shot or interactive** — one-shot spawns a fresh `iris` per generation
  (simplest, reloads the weights every time); interactive keeps one `iris` REPL
  loaded, so repeated generations skip a weight load that costs seconds to
  minutes.
- **Live step preview** — with **Live step preview** on, each denoising step's
  intermediate frame streams into the browser over the same Kitty graphics
  protocol iris uses for terminal display.
- **Schedules** — model default, linear, power curve (with α), shifted sigmoid
  for Flux, FlowMatch Euler for Z-Image.
- **Sessions** — each keeps its own references, takes and settings, and
  switching is instant. **+ New**, **⧉ Duplicate** and **🗑 Delete** are in the
  top bar.
- **Terminal panel** — raw `iris` output streams live; when interactive mode is
  loaded you can type REPL commands (`!explore 4 a cat`, `!help`, …) into it,
  and anything that is not one runs as a shell command — see
  [Security](#security).

## Sessions and state

A session is a directory under the data directory helmstudio gives as `--root`,
holding one line of work: inputs, outputs and the UI state that produced them.
Nothing is shared between sessions, so switching is instant. For an installed
studio that root is `~/.helmstudio/studios/iris-studio/data`; for a checkout
under `helm dev` it is `./.helm/data`. Nothing is written into the repository.

```
sessions/<name>/
├── setting.json     # the session's last prompt and parameters, plus a takes history
├── terminal.log     # this session's terminal output
├── inputs/          # uploaded reference images
└── outputs/         # generated PNGs, each with a <name>.json sidecar of run metadata
```

- **`inputs/`** is the *only* place renders read references from. Uploads,
  takes pulled back with **Use as reference**, and images picked out of
  helmstudio's gallery all land here first.
- **`outputs/`** holds only what `iris` produced: the PNG and its sidecar.
  Deleting the image deletes its sidecar and prunes it from the session's takes
  history.
- `sessions/last_session.json` tracks the last active session and is restored on
  start, creating `session-1` if empty. `sessions/iris.json` and
  `sessions/model.json` remember the paths, so a later launch can omit them.

A finished take becomes helmstudio's too, without changing any of the above: it
is written to `outputs/` as always, and helmstudio adopts it **by hardlink** —
the same bytes under a second name, one inode, counted once. The gallery item
carries the sidecar's parameters plus the session name as `iris_session`, since
helmstudio checks session ids against its own and these are not those. Deleting
a take here does not undo that: the hardlink keeps the bytes and the gallery
item stays, so a take you want gone must go there too.

## A note on interactive mode

`iris_cli.c`'s REPL works over a pipe — it falls back to plain `fgets`-style
line reading when stdin is not a TTY — but a few of its status lines (`Seed:
…`, `Loaded: …`, `Done -> …`) are `printf`'d to stdout without an explicit
flush, so they can sit in libc's block buffer indefinitely when piped rather
than connected to a terminal. The studio works around this instead of patching
iris.c:

- Reference `$N` ids are tracked deterministically — iris assigns them in the
  exact order `!load` and generation commands are processed — so the studio
  never waits on the `Loaded: …` line.
- Completion is detected by polling iris's own temp output directory
  (`/tmp/iris-XXXXXX/`) for a new, size-stable PNG, racing the `Done -> ` line
  in case it does arrive.
- The realised seed of a random request is not reliably captured in interactive
  mode for the same reason. One-shot captures it, because there iris prints it
  to stderr, which is unbuffered.

Live preview frames and step progress are unaffected — iris flushes after each
one. The two modes print progress differently (`  Step 2/4 …` from the one-shot
CLI, `[2/4]:` from the REPL) and the studio reads both.

## Security

iris studio has no authentication, and **less of a guard than a local tool
should have**. Read this before binding it to anything but `127.0.0.1`.

- It binds to `127.0.0.1` by default. Anyone who can reach the port can run
  renders, read every take in every session, and run shell commands.
- **The terminal runs arbitrary shell commands** — anything typed into it that
  is not an interactive `iris` command is handed to `/bin/sh -c` in the
  engine's directory. There is no flag to turn that off. h3 studio gates the
  same feature behind `--allow-shell`; this does not, yet.
- There is **no `Host` check**, so DNS rebinding is not blocked, and **no
  `Origin` or content-type check**. The JSON body is decoded whatever the
  content type says, so a page you visit can send a simple `text/plain` POST —
  no CORS preflight, nothing for the browser to block — and reach any of these
  routes, including the terminal, while you have the studio open.
- Everything the page uses of helmstudio's comes through the same-origin
  `/helm/` proxy, so the browser never holds helmstudio's token. The proxy
  forwards the studio API and the theme stream and nothing else — a launcher
  path (install, launch, stop) 404s and never reaches the daemon.

The first three are worth fixing and are not; they are recorded here rather
than left for you to discover.

## Limits

- One render at a time, deliberately.
- Cancel sends `SIGTERM` to iris's process group, then `SIGKILL` after 2
  seconds; an interactive render is stopped by unloading the model.
- Interactive mode does not reliably report the realised seed — see
  [above](#a-note-on-interactive-mode).
- Progress lags a step: iris writes `  Step 2/4 ` without a newline, so the
  line only arrives when the next step begins with one.
- Recording happens as a take finishes and nothing is reconciled afterwards: a
  take deleted here stays in helmstudio's gallery.
- `/api/queue` does not answer helmstudio's busy contract yet, so its
  switch-studio dialog reads iris studio as unknown rather than busy or idle.
- There is no test for the interactive path; it needs a live `iris` REPL and is
  verified by hand.

## Contributing

Bug reports, feature requests and pull requests are all welcome. Two
conventions worth knowing: no Go dependencies beyond helmstudio's runtime SDK
and `fsnotify`, and no front-end build step — `static/` is served as written.
Run `go test -race ./...` before opening one.

Engine issues — generation quality, speed, Metal kernels, checkpoint support —
belong in [iris.c's own repository](https://github.com/janishar/iris.c).

## License

MIT
