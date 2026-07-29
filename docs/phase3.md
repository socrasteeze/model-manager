# Phase 3 — the presentation layer

Organize by views, not by moving bytes (§9). The app owns a directory tree that
consuming tools point at; nothing points at real files directly, so grouping and
labelling are fully reversible and carry no risk to the library.

---

## Link strategies

```sh
mm link-probe --from /models --to /views/by-base
```

| Strategy | Where | Notes |
|---|---|---|
| **reflink** | btrfs, XFS (FICLONE); APFS (clonefile) | A real file sharing blocks with the original. ~Zero disk cost, indistinguishable from an ordinary file to every consumer, works over SMB with no Samba configuration. |
| **block clone** | Windows ReFS / Dev Drive | `DUPLICATE_EXTENTS_TO_FILE`. Genuinely copy-on-write, unlike a hardlink. |
| **symlink** | anywhere | Cheapest and instant. See the SMB warning. |
| **copy** | anywhere | The only option across filesystems, and the safe Windows default. |
| **hardlink** | within a volume | Ranked *below plain copy*. See the write-through warning. |

### Probed, not inferred

§16.2 makes the point that btrfs subvolumes report different `st_dev` values on
the same filesystem, so comparing device IDs gives a false negative. The probe
attempts each operation on a real temporary file and reports what actually
worked.

### Why hardlink ranks below copy

A hardlink is not a copy; it is a second name for the same inode. A tool that
rewrites a safetensors header in place — precisely the failure §2.1 exists to
survive — writes through the hardlink and mutates the **original**. Reflinks and
ReFS block clones diverge on write; NTFS hardlinks do not.

It is never chosen automatically. `--allow-hardlink` opts in, and the warning is
shown.

### The SMB warning

If any tool reaches the models over a network share, a symlink farm will likely
fail. Samba resolves symlinks server-side only under specific settings, and links
pointing outside the share are disabled by default. **Prefer reflinks.**

### Shared-extent detection

Each reflink reports its full size to `du` and to a naive scan, so a duplicate
report that is not shared-extent aware would loudly announce every intentional
view as wasted space (§9.4). FIEMAP detection lands on Linux; where a platform
cannot answer, the result is marked unsupported so the caller says "unknown"
rather than presenting a zero as a measurement.

---

## Views

```sh
mm view create --name by-base --root /views/by-base --group-by base_model
mm view create --name loras --root /views/loras --group-by tag --type lora
mm view generate by-base
mm view generate by-base --dry-run
mm view delete by-base
```

A view is a saved search, materialized. Group by `flat`, `base_model`, `type`,
`tag` or `collection`.

**Idempotent and reconciling.** Generation adds what is missing, removes what no
longer belongs, and leaves the rest alone — regenerating after one change does not
rewrite the tree.

**Deleting removes only what this app created**, tracked per entry. A view
pointed at a directory that already held something must not destroy it.

**Generating into a scanned model root is refused.** A view inside the scanned
tree would be picked up by the next scan and counted as another copy of every
model in it.

**Provisional paths are excluded.** §10.1 bars a probe-bound path from any
write-side decision, and generating a view is one.

Filenames are sanitized for every platform this targets, including the Windows
reserved names that would otherwise create an unopenable file. Two models sharing
a display name are disambiguated with a hash prefix rather than one silently
losing.
