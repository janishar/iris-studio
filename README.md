# iris studio

A local web control surface for [iris.c](https://github.com/janishar/iris.c) — native
FLUX.2 Klein / Z-Image-Turbo image generation on Apple Silicon (Metal/MPS).

It runs as a [helmstudio][helmstudio] studio, and only that way: helmstudio
installs it, builds it, downloads its weights, gives it a port and a directory
to keep its work in, and starts it. Start at
[Set up helmstudio](#set-up-helmstudio).

iris studio is a Go web server (no JS build step; `fsnotify` for `--dev` hot reload
and helmstudio's runtime SDK) that drives the `iris` binary: it builds
the CLI arguments, runs one-shot or interactive (model-stays-loaded) sessions, manages
reference images and multi-reference combination, queues renders, streams live
denoising-step previews over the Kitty graphics protocol, and keeps per-session state
on disk — all from a browser tab.

## Requirements

- **helmstudio's `helm`** — what runs iris studio, and what installs it. Everything
  below is what its build steps need on the machine;
  see [Set up helmstudio](#set-up-helmstudio).
- macOS on Apple Silicon (Metal/MPS backend; iris.c also builds a BLAS or generic
  backend on other platforms, untested here).
- Go 1.27+, and network for the first build: `go build` fetches one module,
  helmstudio's runtime SDK, through the Go module proxy.
- A built `iris` binary — see below.
- At least one downloaded model directory (`flux-klein-4b`, `zimage-turbo`, etc.).
  helmstudio downloads the one you choose; a checkout links one it already has.

## Building the iris binary

```bash
cd iris.c
make mps        # Apple Silicon Metal GPU (fastest)
```

See [iris.c/README.md](iris.c/README.md) for the BLAS/generic backends and for
downloading model weights (`./download_model.sh 4b`, etc. — pick whichever variant
fits your disk/RAM; ~16-30GB per model).

## Set up helmstudio

iris studio runs under [helmstudio][helmstudio] and does not start without it.
Started by hand it says so and stops:

```
iris studio runs under helmstudio. HELM_API is not set, so there is no platform to run under.
  installed:  start it from helmstudio's Studios list
  a checkout: bash scripts/run.sh
```

So installing it is helmstudio's job, and there is nothing to clone:

1. **Install helm**, helmstudio's launcher:

   ```bash
   /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/janishar/helmstudio/main/installer/install.sh)"
   ```

2. **Install iris studio from helmstudio's Studios list.** It reads this
   repository's own [`helmstudio.yaml`](helmstudio.yaml) and shows you every
   step before it runs any of them — the checkout, the two build commands, the
   checkpoint you pick, and how much it will download.

3. **Start it from helmstudio**, which gives it a port, the checkpoint's path
   and a directory of its own to keep sessions in.

Working *on* iris studio rather than using it means a checkout and `helm dev`:
see [Working on iris studio](#working-on-iris-studio).

[helmstudio]: https://github.com/janishar/helmstudio

## Running under helmstudio

What helmstudio launches is the binary its build steps produced:

```bash
./iris-studio --iris ./iris.c/iris --model <the chosen checkpoint> --port <allocated> --root <its data dir>
```

That is `helmstudio.yaml`'s `processes[0].cmd` with `{models.selected}`,
`{port}` and `{data}` filled in. The flags:

| flag | default | what it is |
| --- | --- | --- |
| `--root` | *(required)* | Directory holding `sessions/` — helmstudio's data directory for this studio. iris studio creates none of its own and will not start without one, and a take has to be inside it for helmstudio to adopt it. |
| `--iris` | *(the last one used)* | The `iris` binary. Also `IRISSTUDIO_IRIS`; changeable later from **⚙ Paths** in the top bar. |
| `--model` | *(the last one used)* | One downloaded checkpoint directory. Also `IRISSTUDIO_MODEL`; changeable later from **⚙ Paths**. |
| `--port` | `8720` | Bind port; helmstudio passes the one it allocated. |
| `--host` | `127.0.0.1` | Bind address. |
| `--dev` | off | Reload the browser when files under `static/` change. |

Then open the studio from helmstudio, or `http://127.0.0.1:<port>` directly.

### What helmstudio adds

The page is the page it always was — the same form, the same preview, the same
takes rail, the same terminal — and helmstudio adds four things around it:

| | |
| --- | --- |
| **Gallery** | The **Gallery** button in the top bar opens helmstudio's own grid over the takes this studio recorded, live — a take that finishes appears without a reload. The grid is scoped to this studio; helmstudio's library is where these sit beside what other studios made. |
| **References from anywhere** | **from gallery**, beside Reference images, picks an image out of that grid and copies it into this session's `inputs/`. It can be another studio's image: that is what a shared gallery is for. |
| **Render log** | A second tab in the Terminal panel, streaming the render as helmstudio sees it — it reconnects after a dropped stream and tells you what it missed. Output's input line and **clear** belong to Output and are hidden while this tab is showing; helm-terminal brings its own. |
| **The launcher** | A render appears in helmstudio as a job with its progress, so what this studio is doing is visible from outside it. |

All of it is fetched through the `/helm/` proxy this server mounts, so the page
holds no token of helmstudio's. The theme and this studio's own colour come the
same way, so the components follow the launcher between light and dark.

## Working on iris studio

This is the developer's path — a checkout, run against helmstudio's platform API
without installing anything into helmstudio. Someone *using* iris studio wants
[Set up helmstudio](#set-up-helmstudio) instead.

```bash
git submodule update --init --recursive
make mps -C iris.c
IRIS_MODEL=/path/to/flux-klein-9b bash scripts/run.sh
bash scripts/run.sh stop
```

The script runs the studio under `helm dev`, which is what gives it the
platform, the port and the data directory. Everything the studio keeps goes in
`./.helm`, and nothing is written into the checkout.

| variable | default | what it does |
| --- | --- | --- |
| `IRIS_MODEL` | *(none)* | A checkpoint directory already on this machine, linked read-only rather than downloaded. Without it, `helm dev` expects the weight to be linked already. |
| `IRIS_WEIGHT` | `flux_klein_9b` | Which of the five declared checkpoints `IRIS_MODEL` is, and which one the studio launches with. |
| `HELM` | `helm` | The `helm` to use, if it is not on `PATH`. |
| `IRIS_DLV` | *(none)* | Listen for a Go debugger on this port. VS Code's **iris studio** launch sets it to 2345 and attaches. |

In VS Code: **run: iris studio** starts it, **stop: iris studio** ends it, and
the **iris studio** launch configuration debugs it.

For front-end work, `--dev` reloads the browser when files under `static/`
change; add it to `processes[0].cmd` in `helmstudio.yaml` while you work.

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
  takes, and settings under `<--root>/sessions/<name>/`, mirroring what you'd get running
  `iris` from different working directories, but switchable from the UI.
- **Terminal panel** — raw `iris` REPL output streams live; when interactive mode is
  loaded you can also type raw commands (`!explore 4 a cat`, `!help`, ...) directly.

## Session layout

Everything below lives under `--root`, helmstudio's data directory for this
studio — `~/.helmstudio/studios/iris-studio/data` for an installed one, `./.helm`
for a checkout under `helm dev`. Nothing is written into the repository.

```
sessions/<name>/
  inputs/         uploaded reference images
  outputs/        generated PNGs, each with a <name>.json sidecar of run metadata
  setting.json    the session's last-used prompt/params, plus a "takes" history
  terminal.log    raw iris output for this session
```

`sessions/last_session.json`, `sessions/iris.json`, and `sessions/model.json` persist
the active session and the last-used `--iris`/`--model` paths across restarts.

A take stays exactly where it is. helmstudio is told about it as well — the file
is adopted by hardlink, the same bytes counted once, and recorded as one gallery
item carrying the parameters it was made from. The session travels with it as a
parameter rather than a `session_id`, because these sessions are still iris
studio's own directories and not helmstudio's.

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
