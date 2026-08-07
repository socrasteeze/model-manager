#!/bin/sh
#
# Model Manager -- update the NAS deployment.
#
# Fetches the current branch from GitHub as a source tarball, builds the
# container, and restarts it. There is no git on this box by design: TOS ships
# without it and the package that would install it is blocked, so the tarball
# API is the sync. GitHub stays authoritative and the NAS holds no git state.
#
#     sh nas-update.sh
#
# or, from a Windows desktop:  nas-refresh.bat
#
# Per-box settings live in ~/.model-manager.env -- see nas-deploy.env.example.

set -eu

# /bin/sh reads a script as it runs, and this one replaces its own directory
# partway through. Re-exec from a copy first, so the interpreter cannot find
# itself rewritten mid-file and die on a syntax error that is not there.
if [ -z "${UPDATER_REEXEC:-}" ]; then
  _self_copy="$(mktemp)"
  cp "$0" "$_self_copy"
  UPDATER_REEXEC=1 exec sh "$_self_copy" "$@"
fi

# --- fixed for this app ------------------------------------------------------

REPO="socrasteeze/model-manager"
BRANCH="${BRANCH:-main}"
APP_NAME="model-manager"
APP_DIR="$HOME/model-manager"

# Container paths, deliberately not configurable.
#
# The scanner records the path it walked, so a model found at /models is stored
# as /models for the life of the library. Where it is mounted from can change;
# where it is mounted TO cannot, or every record reads as absent.
C_MODELS="/models"
C_ARCHIVE="/models-archive"
C_STATE="/state"

# --- per-box settings --------------------------------------------------------

CONF="${MM_NAS_CONF:-$HOME/.model-manager.env}"
if [ ! -r "$CONF" ]; then
  echo "nas-update: no settings at $CONF" >&2
  echo "  copy nas-deploy.env.example there and edit it:" >&2
  echo "    cp $APP_DIR/nas-deploy.env.example $CONF && chmod 600 $CONF" >&2
  exit 1
fi
# shellcheck disable=SC1090
. "$CONF"

for required in MODELS_DIR ARCHIVE_DIR STATE_DIR CONTAINER_USER PORT ALLOW_HOSTS; do
  eval "value=\${$required:-}"
  if [ -z "$value" ]; then
    echo "nas-update: $required is not set in $CONF" >&2
    exit 1
  fi
done

ARCHIVE_INTERVAL="${ARCHIVE_INTERVAL:-6h}"
HOST_PORT="${PORT%%:*}"
CONTAINER_PORT="${PORT##*:}"

command -v docker >/dev/null 2>&1 || { echo "nas-update: docker not found" >&2; exit 1; }

# --- fetch -------------------------------------------------------------------

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> fetching $REPO@$BRANCH"
DL="https://api.github.com/repos/$REPO/tarball/$BRANCH"
if [ -n "${TOKEN_FILE:-}" ] && [ -r "${TOKEN_FILE:-}" ]; then
  curl -fSL -H "Authorization: Bearer $(cat "$TOKEN_FILE")" "$DL" -o "$TMP/app.tar.gz"
else
  # The repo is public, so this needs no credential. TOKEN_FILE stays supported
  # so making it private later is a one-line change in the settings file.
  curl -fSL "$DL" -o "$TMP/app.tar.gz"
fi

tar -xzf "$TMP/app.tar.gz" -C "$TMP"
SRC="$TMP/$(ls "$TMP" | grep -v app.tar.gz | head -1)"

# GitHub wraps the tarball in <owner>-<repo>-<shortsha>. That sha is the only
# version marker a tarball carries -- there is no .git -- so it becomes the
# build stamp, and `mm version` then names the commit that is running.
VERSION="$(basename "$SRC" | sed 's/.*-//')"
echo "    $BRANCH is at $VERSION"

# --- install -----------------------------------------------------------------

echo "==> installing into $APP_DIR"
mkdir -p "$APP_DIR"
if command -v rsync >/dev/null 2>&1; then
  # --delete because a tarball extract only ever adds: without it, a file
  # deleted upstream lingers here forever.
  rsync -a --delete "$SRC"/ "$APP_DIR"/
else
  # No rsync. Wipe and copy is equivalent here precisely because nothing
  # NAS-local lives in this directory -- the settings file and all daemon state
  # are outside it, on purpose.
  rm -rf "$APP_DIR"
  mkdir -p "$APP_DIR"
  cp -a "$SRC"/. "$APP_DIR"/
fi

# --- build -------------------------------------------------------------------

echo "==> building $APP_NAME:$VERSION"
docker build --build-arg VERSION="$VERSION" -t "$APP_NAME:latest" "$APP_DIR"

# --- run ---------------------------------------------------------------------

# A bind mount whose host path does not exist is created as a directory, which
# is how a missing state directory turns into a container that starts, works,
# and loses its database on the next recreate.
mkdir -p "$STATE_DIR" "$MODELS_DIR" "$ARCHIVE_DIR"

ALLOW_HOST_FLAGS=""
for h in $ALLOW_HOSTS; do
  ALLOW_HOST_FLAGS="$ALLOW_HOST_FLAGS --allow-host $h"
done

# Only pass the optional settings that are actually set. The daemon reads these
# straight out of the environment with no empty-string fallback of its own, so
# handing it -e MM_CIVITAI_API="" is not the same as leaving it unset.
ENV_FLAGS=""
for name in CIVITAI_API_KEY HF_TOKEN MM_CIVITAI_API MM_HUGGINGFACE_API MM_CIVARCHIVE_API; do
  eval "value=\${$name:-}"
  if [ -n "$value" ]; then
    ENV_FLAGS="$ENV_FLAGS -e $name=$value"
  fi
done

echo "==> restarting $APP_NAME"
docker stop "$APP_NAME" >/dev/null 2>&1 || true
docker rm   "$APP_NAME" >/dev/null 2>&1 || true

# --serve-files, --allow-archive and --allow-evict are each separate from
# --writable on purpose: that flag has only ever meant "may add things", and
# handing out every model file, fetching from providers on a timer, and deleting
# local copies are three different promises.
#
# --trust-cidr 127.0.0.1/32 is what lets the image's HEALTHCHECK through. Bound
# off-loopback the daemon requires a token from every client and exempts nothing
# for being local, so without it the check gets an honest 401 forever.
#
# shellcheck disable=SC2086 -- the *_FLAGS variables are deliberate word splits
docker run -d --name "$APP_NAME" --restart unless-stopped \
  -p "$PORT" \
  --user "$CONTAINER_USER" \
  $ENV_FLAGS \
  -v "$MODELS_DIR":"$C_MODELS" \
  -v "$ARCHIVE_DIR":"$C_ARCHIVE" \
  -v "$STATE_DIR":"$C_STATE" \
  "$APP_NAME:latest" serve \
    --host 0.0.0.0 \
    --port "$CONTAINER_PORT" \
    --db "$C_STATE/master.db" \
    --writable \
    --serve-files \
    --allow-archive \
    --archive-interval "$ARCHIVE_INTERVAL" \
    --tailnet \
    --trust-cidr 127.0.0.1/32 \
    $ALLOW_HOST_FLAGS

# --- report ------------------------------------------------------------------

echo "==> verifying"

# Wait for it to actually be up rather than reporting on a container that is
# still starting. The daemon exits early and loudly on the mistakes that matter
# here -- a database on a network mount, an unwritable state directory -- so a
# container that is still running after this window has passed those checks.
i=0
status=missing
while [ "$i" -lt 15 ]; do
  if ! status="$(docker inspect -f '{{.State.Status}}' "$APP_NAME" 2>/dev/null)"; then
    status=missing
    break
  fi
  if [ "$status" != "running" ]; then
    break
  fi
  if docker exec "$APP_NAME" mm version >/dev/null 2>&1; then
    break
  fi
  i=$((i + 1))
  sleep 1
done

if [ "$status" != "running" ]; then
  echo "nas-update: container is $status -- it did not stay up." >&2
  docker logs --tail 20 "$APP_NAME" 2>&1 | sed 's/^/    /' >&2
  exit 1
fi

printf '    '
docker exec "$APP_NAME" mm version 2>/dev/null || echo "(started, not answering yet)"

cat <<EOF

  status     $status
  web ui     http://$(hostname):$HOST_PORT/
  database   $STATE_DIR/master.db
  api token  $STATE_DIR/api-token

  First run: open the UI, add $C_MODELS and $C_ARCHIVE under Settings, and scan.
  Those are the container's paths, and they are what the database records.

  To browse this library from another machine, set there:
    MM_UPSTREAM_URL=http://$(hostname):$HOST_PORT
    MM_UPSTREAM_TOKEN=<contents of $STATE_DIR/api-token>

EOF
