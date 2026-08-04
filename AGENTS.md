# Instructions for AI coding agents

## Keep the changelog current

Every commit that changes behavior a user or operator would notice — a fix, a
new feature, a changed default — must update [`CHANGELOG.md`](CHANGELOG.md) in
that same commit. A commit with no visible effect (refactor, test-only change,
comment, formatting) does not need an entry.

Add the new entry under an `## [Unreleased]` heading at the top of the file if
one does not already exist; a release commit later renames that heading to the
version and date and links it to that version's file under
`docs/release-notes/`. Do not wait until a release to write the entry — write
it in the commit that makes the change, while the change is still in front of
you.

Write entries in short, plain sentences: active voice, one fact per sentence,
no marketing language. Look at the existing entries in `CHANGELOG.md` before
adding a new one and match their length and tone.

## Releasing

`CHANGELOG.md` is a running log for anyone reading the repo. It is not the
same file as `docs/release-notes/vX.Y.Z.md`, which is longer, per-release prose
that becomes the GitHub release body (see `.github/workflows/release.yml`).
Cutting a release still needs both: a `docs/release-notes/` file for that
version, and the changelog's `[Unreleased]` section turned into a dated,
linked entry for it. Update `README.md`'s "Current release" line too — it has
gone stale before.
