# Phase 2 — enrichment and download

Section references point into [`../model-manager-spec.md`](../model-manager-spec.md).

---

## Enrichment

```sh
mm enrich                 # everything, throttled and resumable
mm enrich --limit 500     # a bite-sized batch
mm enrich --no-images     # metadata only
```

Models are looked up **by SHA256** — the same key Civitai indexes by. That is the
bonus §2 calls out: an exact lookup, not a name match that could bind the wrong
record to a file.

### The cache is an archive, not an optimization

Models are removed from Civitai regularly, and once gone the metadata is
unrecoverable anywhere (§12.1). So:

- **The full raw response is stored verbatim and never expired.** Extraction runs
  over the stored blob, so improving it never costs another API call and the
  lookup budget is spent exactly once.
- **A later 404 cannot overwrite an archived response.** If the model is gone,
  that copy is the only one left; letting a miss erase it would destroy precisely
  what the spec says to preserve.
- **All hash types are stored**, not just SHA256. AutoV2 is how other tools refer
  to the same file.

Negative results are cached with a TTL. Without one, every run re-queries
thousands of known misses — which is most of a self-trained library. With an
expiring one, a model uploaded later is still found.

### What is not enriched

**Provisional paths.** A probe-bound path has not been confirmed by a full read,
and querying an origin with a hash we are unsure of would archive someone else's
metadata under this file (§10.1).

### HuggingFace

HuggingFace has no hash index (§12), so a file can only be matched by repo and
filename — a claim about a name, not about content. It is recorded at the origin
tier but never writes into `origin_hash`, and its machine tags (`region:`,
`license:`, `base_model:`) are filtered out so they cannot swamp a tag facet
meant for human organization.

### Rate limiting

Throttled with exponential backoff, honouring `Retry-After` when the server sends
it — the server knows better than any curve we would invent. A run that hits a
sustained rate limit stops early and says so, because continuing would just keep
hitting it, and the whole design is resumable.

---

## Downloads

```sh
mm get --dest /models/loras https://civitai.com/api/download/models/12345
mm get --dest /models/loras --sha256 abc123... URL
```

§15 promotes this out of the last phase because it is the single most common
reason people run Stability Matrix's model manager, and calls it its own
workstream rather than a one-liner. The parts that make it one:

- **Resumable** via HTTP range requests, reusing everything already on disk. A
  server that ignores `Range` and re-sends the whole file restarts cleanly rather
  than silently doubling it.
- **Quarantine.** Partial files live in a work directory, never the destination.
  A half-written model sitting in a tool's models folder is one that tool will
  happily load.
- **Checksum verified before admission.** A mismatch quarantines: kept for
  inspection, never published, and reported with both hashes.
- **Never overwrites.** A download landing on a taken name gets a suffix.
- **Auth-gated models.** A 401/403 reports that an API key may be needed rather
  than writing Civitai's HTML login page to disk under a `.safetensors` name.
- **Filename sanitization**, so a server-supplied name cannot traverse out of the
  chosen destination.

A download with no expected hash is accepted on arrival — weaker, and reported as
such. The job records what actually arrived so it can be checked later.
