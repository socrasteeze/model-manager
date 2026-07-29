# Phases 4 and 5 — tiering and projection

---

## Phase 4: SSD tiering

```sh
mm tier stage --cache /ssd/models --capacity-gib 500 --pin <sha256>
mm tier status --cache /ssd/models
mm tier unstage <sha256>
```

The data model already supports this with no new concepts (§16.3): **a staged
copy is a second path on the same hash** — verifiable, disposable, re-derivable at
any time. It is recorded in the index exactly like any other location.

Reflinks do not help here: crossing devices is a genuine copy.

- **Verified by default.** The staged copy is re-hashed before admission. A tier
  copy that silently differs from the original would serve wrong weights, and the
  whole design rests on a path meaning the content its hash claims.
- **The original is never touched.** Unstaging removes only the copy, and refuses
  any path outside the cache root — a corrupted manifest must not become a way to
  delete an original.
- **Pinning plus LRU eviction.** Which policy dominates depends on whether the
  working set is ~150 models or ~600, which is exactly what `mm report`'s size
  distribution exists to tell you. A fully pinned cache reports that it cannot
  make room rather than evicting something you pinned.
- **Provisional paths cannot be staged.** Presenting copied bytes as a model is a
  write-side decision (§10.1).

Cache bookkeeping lives in a manifest beside the data rather than in the master
database, so a cache on a removable disk travels with its own state.

---

## Phase 5: sidecar projection

```sh
mm project --target stability-matrix --dry-run
mm project --target stability-matrix
mm project --target stability-matrix --sha256 <one-model>   # verify a dialect first
```

Consumer sidecars become **derived artifacts** (§4). If a tool mangles one,
regenerate it. Their "pull metadata" button stops being a threat and becomes a
no-op overwritten on the next projection.

Push, not pull (§18) — nothing supports pull today.

| Target | Writes |
|---|---|
| `stability-matrix` | `<model>.cm-info.json` |
| `a1111` | `<model>.json` |
| `swarmui` | `<model>.json` |
| `lora-manager` | `<model>.metadata.json` |

### The generator marker

Every generated file carries `"_generated_by": "model-manager"`. Without it,
projection would have to choose between never overwriting (making regeneration
useless) and always overwriting (destroying what master never captured).

With it: **regenerating repairs a stomped sidecar, while a foreign one is left
alone** unless `--overwrite` is passed.

### Deliberate refusals

- **No "all targets" default.** §15 says start with one tool, verify, then add
  others.
- **`swarmui` + `a1111` together is refused.** Both claim `<model>.json`, so the
  second would silently overwrite the first.
- **Models with no metadata are skipped.** A sidecar carrying only a filename and
  a hash would be read as authoritative emptiness, and a tool would stop showing
  whatever it had worked out for itself.
- **Empty values are omitted rather than written as null**, for the same reason.

Writes are atomic — temporary file plus rename — so a tool reading concurrently
never sees a half-written sidecar, which is the disconnected-metadata symptom
this project exists to eliminate.
