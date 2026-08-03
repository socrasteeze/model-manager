# Phase 6 — Remote browsing and updates

Phases 0–5 all work on files that are already on disk. This phase adds the other
direction: finding models that are *not* here yet, across Civitai, CivArchive and
HuggingFace, and reporting which of the ones already held have a newer version.

## What it does

    mm browse "neon ink" --type lora --base-model Pony
    mm browse --provider civarchive --new-only "portrait"
    mm updates

`browse` searches all three providers and marks every result:

| Status   | Meaning                                                          |
|----------|------------------------------------------------------------------|
| `have`   | A file with this exact content hash is already in the library     |
| `update` | A different version of a model already held, newer than the local |
| `new`    | Nothing local corresponds to it                                   |

`updates` walks the models the library holds a version of and asks each what its
newest version is now.

In the web UI the same thing is the **Browse** tab, with a *Check for updates*
button.

## Why the status column is trustworthy

Every other model browser answers "do I already have this?" by comparing
filenames, which is wrong in both directions: a file you renamed looks new, and a
different file that happens to share a name looks owned.

Here every local file is already keyed by its SHA256 (§2) and every provider
reports a SHA256 for its files, so the question is a hash lookup. A renamed file
is still recognised; a same-named different file is not confused for it.

## Providers

| Provider    | Search | Hashes in results | Notes                              |
|-------------|--------|-------------------|------------------------------------|
| Civitai     | yes    | yes               | One listing per model *version*    |
| CivArchive  | yes    | yes               | Mirrors records removed upstream   |
| HuggingFace | yes    | via a second call | LFS oid is the content SHA256      |

### Civitai

Listings are flattened to the model *version*, not the model. A model is an
abstract thing with no hash and nothing to download; a version has files, a base
model, trigger words and an identity comparable against what is on disk. Every
version is emitted, not just the newest — an older version is frequently the one
wanted, and it is what makes an owned-older-version detectable.

The API root is configurable through `MM_CIVITAI_API`. `civitai.com`,
`civitai.red` and `civitai.green` serve the same API over different catalogues
(`.red` being the adult split), so the host decides what a search can return.

Point at the host you want directly rather than relying on a redirect: Go strips
the `Authorization` header when a redirect crosses to a different registered
domain, so a `.com` → `.red` hop would silently drop the API key and fail as a
bare 401.

### HuggingFace

`internal/origin/huggingface.go` notes that HuggingFace has no hash index, and
for *reverse* lookup that is true — a local file cannot be identified by asking
HuggingFace what it is.

But the tree endpoint exposes each file's LFS object id, and for LFS-backed
files that oid **is** the SHA256 of the content. So already-have detection is
exact on HuggingFace too, and a download can be checksum-verified against a hash
obtained independently of the bytes.

Search results do not include hashes, so `browse` makes one extra call per
HuggingFace listing to fetch them. Without it every HuggingFace result would
report as `new` even when the file was already on disk — exactly the duplicate
download this project exists to prevent. `--no-resolve` skips it.

Small non-LFS files carry a git blob SHA1 instead, and are deliberately left
without a hash rather than having a SHA1 recorded in a SHA256 field.

### CivArchive

CivArchive mirrors Civitai records including removed ones, which makes it the
provider that matters most for this project's purpose: §12.1 already treats the
local archive as possibly the last surviving copy of a taken-down model's
metadata, and CivArchive is the one place a record that was never captured can
still be recovered from. Records with a deletion date surface it in the listing.

> **Endpoint confirmed; field shapes still being learned.** The build
> environment blocks outbound connections to `civarchive.com`, so the search
> path was written against a guess. A live run against the real service
> confirmed the endpoint and query shape are right — it returned well-formed
> JSON — but surfaced a field-typing bug: some id fields arrive as strings
> (`"v9208"`) rather than numbers, which the original `json.Number`-typed
> decoder rejected outright, failing the whole page over one field. Fixed with
> a decoder that accepts either and decodes each record independently, so one
> unfamiliar field costs that record rather than the page. The paths still live
> in one template block (`civArchivePaths` in `internal/origin/civarchive.go`)
> for further correction as more of the response shape is confirmed.

## Credentials

Read from the environment, never stored in the database — a token in the master
DB would end up in every backup and every copy of the library.

| Variable                                | Used for                        |
|-----------------------------------------|---------------------------------|
| `CIVITAI_API_KEY`                       | Gated Civitai models            |
| `HF_TOKEN` / `HUGGING_FACE_HUB_TOKEN`   | Gated or private HF repos       |
| `MM_CIVITAI_API`                        | Civitai API root                |
| `MM_HUGGINGFACE_API`                    | HuggingFace API root            |
| `MM_CIVARCHIVE_API`                     | CivArchive API root             |

An API key, not OAuth. OAuth exists so an application can act on *other people's*
accounts; here the operator is the account holder, so there is nothing to
delegate. A distributed binary also cannot hold a confidential client secret, and
a browser round trip would break `mm enrich` running headless over SSH.

**Credentials are scoped by host.** One client talks to three unrelated third
parties, and a single `Authorization` header applied to every request would hand
the Civitai key to HuggingFace on the first cross-provider search. `TokenFor`
selects per host with exact matching, so a lookalike domain cannot claim a token
that is not its own. Downloads use the same selection — the transfer path had
its own single key field and did leak, which is fixed.

**With a token set, the UI also gets a cookie.** Opening `/?token=<token>`
mints an `HttpOnly; SameSite=Strict` cookie, because the page's own script and
stylesheet requests are issued by the browser and carry no header or query
parameter — without it the UI is a blank page in exactly the off-loopback
deployment the token exists for.

## HTTP API

    GET    /api/browse?q=&provider=&type=&base_model=&sort=&page=&nsfw=
    GET    /api/updates
    GET    /api/remote-image?url=
    POST   /api/downloads          -> 202 {status,id,dest_dir} | 409 {id} if running
    GET    /api/downloads
    DELETE /api/downloads/{id}     -> cancels in flight, forgets when terminal
    GET    /api/downloads/roots
    GET    /api/downloads/destination?root=&type=

These are the only endpoints that talk to a **third party**, and
`mm serve --no-remote` disables them, which is how an operator keeps the daemon
off the public internet without needing a firewall rule.

Phase 7 added endpoints that make outbound requests to a *local* service — a
ComfyUI address the operator typed into their own settings — for rendering
thumbnails. Those are deliberately not covered by `--no-remote`, which exists to
stop the daemon contacting Civitai, HuggingFace and CivArchive, not to stop it
contacting a service of yours. With no address configured nothing is contacted
at all. See [phase7](phase7.md).

`/api/remote-image` proxies provider thumbnails. The page's CSP is
`img-src 'self' data:`, so a remote URL in an `<img>` is refused outright and
every preview would silently fail; loading them directly would also have the
browser contact the provider CDN on every search, disclosing the viewer's address
and browsing to a third party the daemon was otherwise mediating.

Because it is an outbound fetcher driven by a client-supplied URL — an SSRF
primitive if left open — the host must be a configured provider host or a known
provider CDN, redirects are re-checked against the same rule, the body is size
capped, and the content is sniffed rather than trusted from the response header.
Nothing is persisted: these are previews for models that are not owned, and
writing them into the blob store would fill it with images for files that were
never downloaded.

## Downloading from the UI

`POST /api/downloads` starts a transfer; `GET /api/downloads` reports progress
for polling. The Browse tab shows a destination picker, a Download button on
anything not already owned, and a progress bar per transfer.

This is the most dangerous endpoint in the daemon — left naive it is a remote
primitive for "fetch an arbitrary URL and write it anywhere on this filesystem"
— so three independent checks run before any byte moves.

1. **It is a mutation.** Refused in read-only mode like any other write, and the
   manager is only constructed when the daemon is started `--writable`.
2. **The source host must be a known provider.** A URL is not accepted merely
   for being well-formed. Matching is by domain suffix, so `civitai.com` passes
   and `civitai.com.attacker.net` does not.
3. **The destination must already be a managed model root.** It is never
   inferred from the URL, the filename, or a default. The server publishes the
   legal roots at `/api/downloads/roots` and the client picks one; anything
   else is refused. A requested subdirectory has traversal segments stripped,
   and containment is re-checked after symlink resolution, so a subdirectory
   that is a symlink out of the tree cannot be used to escape.

Phase 7 moved the *subfolder* decision to the server as well. The browser used
to pluralize the provider's type string into `${type}s` and send it — which
fabricated directory names from unvalidated input and assumed one folder
vocabulary when the three tools in use have three. The subfolder now comes from
(root, type) server-side, and `/api/downloads/destination` reports the resolved
path so the UI can show where a file will land before it is fetched. See
[phase7](phase7.md).

The standing guarantee still holds: nothing existing is modified. A download
landing on a name already taken is given a new name rather than replacing what
is there, and a file whose hash does not match what was expected is left in the
quarantine directory and never published into the model root at all.

One limit worth stating plainly: the host check constrains where the request is
*sent*. Transfers follow redirects — they must, since HuggingFace resolve URLs
redirect to per-region CDN hosts — and redirect targets are not re-checked. So
this bounds who you can ask, not every host ultimately contacted. What makes
that acceptable is that the response is written to a quarantine file and
verified against an expected hash rather than returned to the caller.

A completed download is indexed immediately through the same tier-3 path a scan
uses (`scan.IndexFile`), so it appears in the library without a manual rescan
and browse flips it from `new` to `have`. Sharing that code path matters: a
separate implementation would give the file a subtly different identity — a
different weights hash, or a path row missing the `(device, inode)` key that
makes re-scans cheap — and it would surface later as a phantom duplicate. The
root recorded is the canonical one the allowlist matched, never the caller's
spelling of it, and an indexing failure lands on the job as `index_error`
rather than disappearing.

### Job lifecycle

    pending -> downloading -> verifying -> complete
                   |              |-> quarantined   (hash/size mismatch)
                   |-> failed                        (partial kept, resumable)
                   |-> cancelled                     (partial kept, resumable)

A job's ID is a hash of URL and filename, which is what lets a retry resume the
same partial file. Starting an ID that is already in flight is refused with 409
and the running job's ID: two transfers appending to one partial interleave
their streams into byte garbage, which is silent, multi-gigabyte, and lands in
a model root. Once terminal, the same ID may be started again — that is the
Retry button.

Quarantine moves the rejected bytes to `<id>.quarantine` rather than leaving
them on the resume path. Keeping them there meant every future attempt at that
URL resumed from poisoned bytes and failed forever, with no way to clear it
from the UI. The bytes are still kept; they just no longer poison the retry.

When no expected hash is available the promised size is checked instead. It is
weaker than a hash but catches the common failure: an HTML login page served
with a 200, published under a `.safetensors` name.

## What listings are not

A `Listing` is a claim about a file that does not exist here yet. Nothing a
search returns is ever recorded as a field observation. Metadata only earns
provenance after the bytes are downloaded and hashed, at which point the ordinary
Civitai/HuggingFace extraction path runs against a real local SHA256.

This is why browsing cannot corrupt the library no matter what a provider
returns.

## Downloading

`browse` and `updates` print a ready-to-run `mm get` command for anything not
already held; the UI offers the same as a copy button. `mm get` is the Phase 2
downloader: resumable, quarantined until verified, checksum-checked, and it never
overwrites an existing file.

Owned results deliberately do not get a download command.

## Known gaps

- Redirect targets during a transfer are not host-checked (see above).
- Base-model filters are approximate. Civitai wants its own labels, so a
  normalized name expands to the set it covers; HuggingFace tags are full repo
  names, so its filtering happens client-side after normalization.
- `mm updates --limit N` always checks the same first N models; there is no
  resume cursor yet.
- Update checking covers Civitai only. HuggingFace repos have no version
  identity, so "newer" would have to mean `lastModified` moving, which is a
  weaker claim than a version id changing.
- CivArchive field shapes are still being learned (see above).
