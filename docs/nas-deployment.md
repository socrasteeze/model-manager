# Running the library on a NAS

The machine that holds the collection is the one worth running continuously. It
serves model files to every other machine (`--serve-files`), archives from the
providers so a takedown cannot erase a model (`--allow-archive`), and answers
every local route with the internet unplugged.

This describes running it in a container on a TerraMaster or Synology NAS,
updated from GitHub with one command. The same shape works anywhere Docker does.

## Why a tarball and not a clone

TOS and DSM ship without git, and the package that would install it is blocked.
So the NAS never clones: `nas-update.sh` fetches the branch as a source tarball
from GitHub's REST API, extracts it, builds the image and restarts the
container. That download *is* the sync. GitHub stays authoritative and the NAS
holds no git state to drift.

The build needs a Go toolchain and nothing else — the web UI is committed under
`internal/webui/dist` and embedded by the compiler, so no Node ever runs on the
NAS.

## One-time setup

On the NAS:

```sh
mkdir -p ~/model-manager-deploy && cd ~/model-manager-deploy
curl -fSL -o nas-update.sh \
  -H "Accept: application/vnd.github.raw" \
  "https://api.github.com/repos/socrasteeze/model-manager/contents/nas-update.sh?ref=main"
```

`model-manager-deploy`, not `model-manager`. This directory is deleted and
rewritten on every update, and the shorter name is exactly where a working
checkout of the same project tends to live. The script refuses to run if it
finds a `.git` inside `APP_DIR`, but the naming is the first line of defence
and the refusal is the second.

Fetch it there rather than copying it through Windows — a copy that picks up CRLF
makes `/bin/sh` read `set -eu\r` and fail with `: invalid option`, which names
neither the file nor the cause. (`.gitattributes` pins `*.sh` to LF so a
checkout or tarball extract cannot introduce one; a manual copy still can.)

Then the per-box settings, which live outside the repo so `rsync --delete`
cannot wipe them on the next update:

```sh
cp ~/model-manager/nas-deploy.env.example ~/.model-manager.env
chmod 600 ~/.model-manager.env
```

Edit it — every value is documented in the file — and run:

```sh
sh ~/model-manager/nas-update.sh
```

From a Windows desktop after that, `nas-refresh.bat` does the same thing over
SSH. Set `MM_NAS_HOST` once with `setx` and reopen the terminal.

## Five things that will otherwise cost you an afternoon

**Bridge networking hides who is calling.** Docker's userland proxy rewrites the
source address of every published connection to the bridge gateway, so the
daemon sees `172.17.0.1` for every client no matter how they connected. Any
decision made from the caller's address then collapses: `--tailnet` never
matches, and the only `--trust-cidr` that would satisfy it is the whole bridge —
which exempts every host that can reach the port while reading as though it
exempted only the tailnet. That is the worst kind of security setting, one that
looks narrow and is not.

This deploys with `--network host` for that reason, not for speed. The real
address survives, so "tailnet clients skip the token, everyone else presents
one" means what it says. The cost is that the port is claimed directly on the
host, so `PORT`'s mapping no longer applies and nothing else may hold it.

**The daemon rejects host names it was not told about.** `ALLOW_HOSTS` must list
every name and address the UI is opened by — the short name, any `.local` or
tailnet name, and the bare IP if you ever type one. This is the DNS-rebinding
defence: a browser tab on any site can resolve a name it controls to your
address and issue same-origin requests, and rejecting unexpected `Host` values
is what closes it. The daemon cannot know its own names. A missing entry looks
exactly like the server being down — the connection succeeds and every request
is refused.

**Container paths become permanent database state.** The scanner records the
path it walked, so a model found at `/models` is stored as `/models` for the
life of the library. Where those mount *from* can change freely; where they
mount *to* cannot. That is why the container paths are fixed in `nas-update.sh`
rather than configurable.

**`STATE_DIR` needs room for a whole model.** A download is written there in
full and only then moved to its model folder, so a 12 GB fetch needs 12 GB free
in the state directory as well as at the destination. The daemon checks both
before starting and refuses a transfer that cannot fit, with both numbers, in
the first second. Put the state directory on the large volume, not a system
partition.

**The database must be on local storage.** SQLite's locking is unreliable over
network filesystems and putting the database on one is a known corruption
vector, so the daemon refuses to start if it finds one. A bind mount of the
NAS's own array is local and passes. If it ever refuses on storage you know is
local, the message names the filesystem it found, and `--allow-network-db` is
the deliberate override — reach for it only once you are sure the answer is
wrong.

## First run

Open the UI, add `/models` under Settings, and scan. That is the container's
path, and it is what the database records.

`ARCHIVE_DIR` mounts a second library at `/models-archive` and is optional.
Think before pointing it at a directory another tool owns, because **every
mounted root is somewhere the daemon may write** — a download or an archive
intake can be sent to any enabled root. A sync tool's versioning folder is the
trap: what this daemon captures there as a permanent copy, the other tool is
free to prune on its own schedule. Give the archive a directory of its own.

Indexing such a folder without letting anything write to it is still possible —
add it, scan once, then disable it in Settings. A disabled root keeps every path
and record it contributed but drops out of the destination list, which is what
makes the duplicate report usable on a folder you do not control.

The API token is generated on first start at `$STATE_DIR/api-token`. A token is
mandatory whenever the daemon is bound anywhere but loopback — anything that can
reach the port can otherwise read every model file the daemon can see. Tailscale
clients skip it, because `--tailnet` trusts addresses that were authenticated
before the packet arrived.

## Pointing other machines at it

On each client:

```
setx MM_UPSTREAM_URL "http://<nas-hostname>:8737"
setx MM_UPSTREAM_TOKEN "<contents of the api-token file>"
setx MM_UPSTREAM_NAME "Library"
```

The library then appears in Browse beside Civitai, CivArchive and HuggingFace,
with the same have/new marks against the client's own disk. **Pull** copies a
model down, verified against the hash the NAS indexed it under; **evict** — with
`--allow-evict` on the client — removes the local file and keeps everything
known about it, so the model stays listed as available from the NAS.

Only the NAS needs a provider API key, and only the NAS spends the rate limit.

## Updating

```
nas-refresh.bat
```

It rebuilds from the current `main` and recreates the container. The database,
previews and token live in the state mount, so nothing is lost. `mm version`
reports the short commit sha the running container was built from — the tarball
carries no `.git`, so `nas-update.sh` takes it from the directory name GitHub
wraps the archive in and passes it to the build.

## Troubleshooting

| Symptom | Cause |
|---|---|
| Every request refused, connection fine | The `Host` you used is not in `ALLOW_HOSTS` |
| Container reports unhealthy forever | `--trust-cidr 127.0.0.1/32` missing; the healthcheck gets an honest 401 |
| 401 in a browser from a tailnet machine | Bridge networking is laundering the source address into `172.17.0.1`, so `--tailnet` cannot match. Use `NETWORK_MODE="host"` |
| `: invalid option` running the script | CRLF line endings — refetch it on the NAS, or `tr -d '\r' < nas-update.sh > f && mv f nas-update.sh` |
| Permission errors on every write | `CONTAINER_USER` does not own the mounts — check `ls -n` |
| Mount is empty instead of your models | Path case: TOS uses `/Volume1`, DSM `/volume1` — check `df -h` |
| Refuses to start, names a filesystem | The database landed on a network mount; move `STATE_DIR` to local storage |
| Downloads refused with two numbers | Genuinely out of space, in the state directory or at the destination |
| Library empty after an update | The mounts moved to different container paths; put them back |
| Refuses to run, names a git checkout | `APP_DIR` points at a clone; this wipes that directory, so point it elsewhere |
| Complains a setting is unset that you deliberately removed | The installed script checks settings before it updates itself, so a change to which settings are required cannot self-apply. Re-run the bootstrap `curl` once, then update as usual |
