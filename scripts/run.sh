#!/usr/bin/env bash
# Launch iris studio from a checkout, under `helm dev`: it keeps everything the
# studio keeps in ./.helm, passes that directory as --root, and serves helm-css,
# the theme and this studio's hue through the /helm/ proxy the server mounts.
# The studio does not start without it — there is no standalone mode.
#
#   [IRIS_MODEL=<checkpoint dir>] [IRIS_WEIGHT=<name>] [HELM=<helm>] [IRIS_DLV=<port>] \
#     bash scripts/run.sh [helm dev flags]
#   bash scripts/run.sh stop
#
# helm comes from helmstudio's installer and helm-runtime-sdk from the Go
# module proxy, so neither needs a helmstudio checkout. What the script makes
# itself (the debugger's shim, its pid file) stays in .cache/iris-studio and
# dist/, beside the .helm helm dev keeps. What each variable does: README.md,
# "Running under helmstudio".
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

RUN="$PWD/.cache/iris-studio"
HELM="${HELM:-helm}"
PIDFILE="$RUN/iris-studio.pid"
# Which of the five checkpoints this run launches with. The manifest declares
# them all selectable and the command reads {models.selected}, so one has to be
# chosen; the launcher's approval screen is where a user chooses, and -select
# is where a checkout does.
WEIGHT="${IRIS_WEIGHT:-flux_klein_9b}"

# Ends the helm dev this script last started, which stops the studio before it
# exits. A run starts by doing the same, so starting again is a restart.
stop() {
  local pid
  pid="$(cat "$PIDFILE" 2>/dev/null || true)"
  if [ -n "$pid" ] && ps -p "$pid" -o command= 2>/dev/null | grep -q "dev -f helmstudio.yaml"; then
    echo "helm dev: stopping the iris studio started before ($pid)"
    kill -TERM "$pid" 2>/dev/null || true
    while kill -0 "$pid" 2>/dev/null; do sleep 0.2; done
  fi
  rm -f "$PIDFILE"
}
if [ "${1:-}" = "stop" ]; then
  stop
  exit 0
fi

if ! command -v "$HELM" >/dev/null; then
  echo "helm is not installed. Install it with helmstudio's installer, or set HELM to its path:" >&2
  echo '  /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/janishar/helmstudio/main/installer/install.sh)"' >&2
  exit 1
fi
if [ ! -x iris.c/iris ]; then
  echo "iris.c/iris is not built: git submodule update --init --recursive, then make mps -C iris.c" >&2
  exit 1
fi

if [ -n "${IRIS_MODEL:-}" ]; then
  if [ ! -d "$IRIS_MODEL" ]; then
    echo "weights: no checkpoint at $IRIS_MODEL" >&2
    exit 1
  fi
  # helm dev records where a weight is linked and links per file, so the
  # directory itself is never written to and nothing is downloaded.
  echo "weights: linking $IRIS_MODEL as $WEIGHT"
  set -- -link "$WEIGHT=$IRIS_MODEL" "$@"
fi
# Without this, a studio with selectable weights and no choice is refused
# rather than launched with an arbitrary one. The choice persists in .helm, so
# passing it every run only repeats what the last one recorded.
#
# -select is newer than helmstudio 1.0.0-rc.3, and iris studio is the only
# studio that needs it — so a helm from before it cannot launch this one from a
# checkout at all, and saying so here beats "flag provided but not defined".
if ! "$HELM" dev -h 2>&1 | grep -q -- "-select"; then
  echo "this helm has no 'dev -select', and iris studio cannot be launched from a checkout without it:" >&2
  echo "  every one of its five checkpoints is selectable, so {models.selected} has nothing to resolve to." >&2
  echo "  update helm, or build one from a helmstudio checkout and point HELM at it." >&2
  exit 1
fi
set -- -select "$WEIGHT" "$@"

# helm dev runs no build[] — the checkout is the developer's, and its own
# source says so — so it launches whatever ./iris-studio happens to be. A
# stale one is the difference between the /helm/ proxy answering and 404ing.
# A debug run leaves ./iris-studio as a shell script, and `go build -o` refuses
# to overwrite a file that is not an object file — so the last run's shim goes
# before this one builds, whichever kind of run this is.
rm -f iris-studio iris-studio.bin
BUILD=(go build -o iris-studio)
[ -n "${IRIS_DLV:-}" ] && BUILD=(go build -gcflags 'all=-N -l' -o iris-studio)
echo "build: ./iris-studio${IRIS_DLV:+ (for the debugger)}"
"${BUILD[@]}" .

if [ -n "${IRIS_DLV:-}" ]; then
  # helm dev hands the studio only a restricted environment, so a request to
  # listen for a debugger cannot reach it as a variable — and the manifest
  # names ./iris-studio, not a debugger. The binary moves aside and that name
  # becomes a shim that runs it under Delve, which listens for the editor. A
  # later run without IRIS_DLV builds over the shim again.
  if ! command -v dlv >/dev/null; then
    echo "dlv is not installed: go install github.com/go-delve/delve/cmd/dlv@latest" >&2
    exit 1
  fi
  # --continue matters: without it Delve halts the program at entry and waits
  # for a client, the studio never listens, and helm dev fails it on
  # health.timeout_s 30 seconds later.
  mv iris-studio iris-studio.bin
  printf '#!/bin/sh\nexec dlv exec --headless --continue --accept-multiclient --api-version=2 --listen="127.0.0.1:%s" "%s/iris-studio.bin" -- "$@"\n' \
    "$IRIS_DLV" "$PWD" >iris-studio
  chmod +x iris-studio
  echo "dlv: the studio listens for a debugger on 127.0.0.1:$IRIS_DLV"
fi

stop
mkdir -p "$RUN"
echo $$ >"$PIDFILE"
exec "$HELM" dev -f helmstudio.yaml "$@"
