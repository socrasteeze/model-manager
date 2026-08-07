# Model Manager as a container, for the machine that holds the collection.
#
# The web UI is committed under internal/webui/dist and embedded by the Go
# compiler, so this needs a Go toolchain and nothing else. No Node stage, no
# npm install on a NAS.

ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

# Dependencies as their own layer, so a source-only change reuses the module
# download. On a NAS pulling over its own uplink that is the difference between
# a twenty-second rebuild and a three-minute one.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Stamped the way .github/workflows/release.yml stamps a release build, so
# `mm version` names the commit that is actually running. A source tarball
# carries no .git, so the value has to be passed in from outside.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/mm ./cmd/mm

# Tracks the current stable 3.x rather than pinning. A pin that has aged out of
# the registry fails the deploy on a box that cannot easily be debugged, which
# is the worse failure for something meant to run unattended for years.
FROM alpine:3

# ca-certificates is not optional. Enrichment, browsing and archiving all talk
# HTTPS to Civitai and HuggingFace, and without it every one of them fails with
# a certificate error that reads like a network fault. tzdata so the archive
# scheduler's timestamps are legible; wget for the healthcheck below.
RUN apk add --no-cache ca-certificates tzdata wget

COPY --from=build /out/mm /usr/local/bin/mm

EXPOSE 8737

# /api/health answers from the database and the disk, so it stays true with the
# internet unplugged -- which is the point of a healthcheck on this daemon.
#
# It needs `--trust-cidr 127.0.0.1/32` on the serve command to pass. A daemon
# bound off-loopback requires a bearer token from every client, and nothing is
# exempt by being local; see internal/api/security.go. Without that flag this
# check gets an honest 401 and reports the container unhealthy forever.
# Reads MM_HEALTH_PORT so the check follows a daemon moved off the default,
# which host networking makes possible -- there the published port is whatever
# the daemon binds, with no mapping to hide a mismatch.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:${MM_HEALTH_PORT:-8737}/api/health" || exit 1

# Entrypoint rather than a baked command, so `docker run <image> serve --flags`
# reads exactly like the CLI does everywhere else, and `docker exec <name> mm
# version` works without a shell.
ENTRYPOINT ["mm"]
