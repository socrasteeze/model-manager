// Package tier stages hot models onto fast storage.
//
// The data model already supports this with no new concepts (spec §16.3): an SSD
// copy is a second path on the same hash -- verifiable, disposable, and
// re-derivable at any time. Nothing here invents a parallel notion of identity.
//
// The mechanism is: copy to the SSD in the background, then atomically swap the
// presentation entry to point at the copy. The swap is instant; the only cost is
// background copy time, which is what satisfies the limited-downtime requirement.
// Reflinks do not help here -- crossing devices is a genuine copy.
package tier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/socrasteeze/model-manager/internal/hashing"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/timestamp"
)

// Entry is one staged model.
type Entry struct {
	SHA256     string `json:"sha256"`
	Name       string `json:"name,omitempty"`
	SourcePath string `json:"source_path"`
	CachePath  string `json:"cache_path"`
	Bytes      int64  `json:"bytes"`
	Pinned     bool   `json:"pinned"`
	LastUsed   string `json:"last_used,omitempty"`
	StagedAt   string `json:"staged_at"`
}

// Status describes the cache.
type Status struct {
	Root        string  `json:"root"`
	CapacityGiB float64 `json:"capacity_gib"`
	UsedBytes   int64   `json:"used_bytes"`
	Entries     []Entry `json:"entries"`
	Pinned      int     `json:"pinned"`
}

// Manager owns a fast-storage cache directory.
type Manager struct {
	st   *store.Store
	root string

	// CapacityBytes bounds the cache. Zero means unbounded, which is only sane
	// when the cache has a disk to itself.
	CapacityBytes int64
}

// NewManager opens a tier cache rooted at dir.
func NewManager(st *store.Store, dir string, capacityBytes int64) (*Manager, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("tier: resolving cache root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("tier: creating cache root: %w", err)
	}
	return &Manager{st: st, root: abs, CapacityBytes: capacityBytes}, nil
}

// Root is the cache directory.
func (m *Manager) Root() string { return m.root }

// StageOptions configures staging.
type StageOptions struct {
	Pin bool

	// Verify re-hashes the staged copy before it is admitted. On by default via
	// the CLI: a tier copy that silently differs from the original would serve
	// wrong weights, and the whole design rests on a path meaning the content
	// its hash claims.
	Verify bool

	Progress func(copied, total int64)
	Logf     func(format string, args ...any)
}

// Stage copies a model to fast storage and records the copy as another path on
// the same hash.
func (m *Manager) Stage(ctx context.Context, sha string, opts StageOptions) (*Entry, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	source, size, err := m.confirmedSource(sha)
	if err != nil {
		return nil, err
	}

	if existing, err := m.entry(sha); err == nil && existing != nil {
		if _, statErr := os.Stat(existing.CachePath); statErr == nil {
			// Already staged. Touching it keeps LRU honest without recopying
			// gigabytes.
			_ = m.touch(sha)
			if opts.Pin {
				_ = m.SetPinned(sha, true)
			}
			return existing, nil
		}
		// The record says staged but the file is gone -- forget it and restage.
		_ = m.forget(sha)
	}

	if m.CapacityBytes > 0 {
		if err := m.evictFor(size, logf); err != nil {
			return nil, err
		}
	}

	dest := m.cachePath(sha, source)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, fmt.Errorf("tier: preparing cache directory: %w", err)
	}

	// Copy to a temporary name and rename into place. A consuming tool must
	// never see a partially copied model under its final name -- it would load
	// it and fail in a way that looks like a corrupt download.
	tmp := dest + ".staging"
	if err := copyWithProgress(ctx, source, tmp, opts.Progress); err != nil {
		os.Remove(tmp)
		return nil, err
	}

	if opts.Verify {
		res, err := hashing.New(0, 0).Full(tmp)
		if err != nil {
			os.Remove(tmp)
			return nil, fmt.Errorf("tier: verifying staged copy: %w", err)
		}
		if res.SHA256 != sha {
			os.Remove(tmp)
			return nil, fmt.Errorf(
				"tier: staged copy of %s hashed to %s — the copy is not the model, refusing to admit it",
				short(sha), short(res.SHA256))
		}
	}

	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("tier: publishing staged copy: %w", err)
	}

	entry, err := m.record(sha, source, dest, size, opts.Pin)
	if err != nil {
		return nil, err
	}
	logf("staged %s (%s)", short(sha), humanBytes(size))
	return entry, nil
}

// Unstage removes a staged copy.
//
// This deletes only a file this app created inside its own cache directory. The
// original is untouched, which is what makes a tier copy disposable.
func (m *Manager) Unstage(sha string) error {
	entry, err := m.entry(sha)
	if err != nil {
		return err
	}
	if entry == nil {
		return nil
	}
	if !isUnder(entry.CachePath, m.root) {
		// Refuse to delete anything outside the cache. A corrupted record must
		// not become a way to remove an original.
		return fmt.Errorf("tier: refusing to remove %s: it is outside the cache root", entry.CachePath)
	}
	if err := os.Remove(entry.CachePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("tier: removing staged copy: %w", err)
	}
	return m.forget(sha)
}

// SetPinned marks an entry as never-evictable.
func (m *Manager) SetPinned(sha string, pinned bool) error {
	entry, err := m.entry(sha)
	if err != nil || entry == nil {
		return err
	}
	entry.Pinned = pinned
	return m.save(*entry)
}

// Touch records a use, which is what LRU eviction orders by.
func (m *Manager) Touch(sha string) error { return m.touch(sha) }

// Status reports the cache contents.
func (m *Manager) Status() (*Status, error) {
	entries, err := m.entries()
	if err != nil {
		return nil, err
	}
	s := &Status{Root: m.root, Entries: entries}
	if m.CapacityBytes > 0 {
		s.CapacityGiB = float64(m.CapacityBytes) / float64(1<<30)
	}
	for _, e := range entries {
		s.UsedBytes += e.Bytes
		if e.Pinned {
			s.Pinned++
		}
	}
	return s, nil
}

// evictFor makes room for a file of the given size.
//
// Pinned entries are never evicted; everything else goes least-recently-used
// first. Which policy dominates depends on whether the working set turns out to
// be ~150 models or ~600, which is precisely the number Phase 0's report exists
// to produce (§16.3).
func (m *Manager) evictFor(needed int64, logf func(string, ...any)) error {
	entries, err := m.entries()
	if err != nil {
		return err
	}

	var used int64
	for _, e := range entries {
		used += e.Bytes
	}
	if used+needed <= m.CapacityBytes {
		return nil
	}

	evictable := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if !e.Pinned {
			evictable = append(evictable, e)
		}
	}
	sort.Slice(evictable, func(i, j int) bool {
		return evictable[i].LastUsed < evictable[j].LastUsed
	})

	for _, e := range evictable {
		if used+needed <= m.CapacityBytes {
			break
		}
		if err := m.Unstage(e.SHA256); err != nil {
			return err
		}
		used -= e.Bytes
		logf("evicted %s (%s) to make room", short(e.SHA256), humanBytes(e.Bytes))
	}

	if used+needed > m.CapacityBytes {
		return fmt.Errorf(
			"tier: cannot fit %s: %s of the %s cache is pinned",
			humanBytes(needed), humanBytes(used), humanBytes(m.CapacityBytes))
	}
	return nil
}

// confirmedSource finds a present, confirmed path for a model.
//
// Provisional paths are excluded: staging copies bytes and then presents them as
// that model, which is a write-side decision, and §10.1 bars a probe-bound path
// from those. The rule now lives in the store, because serving a model file over
// HTTP needs exactly the same one and two copies would eventually disagree.
func (m *Manager) confirmedSource(sha string) (string, int64, error) {
	row, size, err := m.st.ConfirmedPresentPath(sha)
	if err != nil {
		return "", 0, fmt.Errorf("tier: no confirmed on-disk copy of %s to stage", short(sha))
	}
	return row.Path, size, nil
}

// cachePath keeps the original filename so a consuming tool pointed at the cache
// sees the names it expects, while sharding by hash prefix to avoid one enormous
// directory.
func (m *Manager) cachePath(sha, source string) string {
	return filepath.Join(m.root, sha[:2], sha[:12]+"-"+filepath.Base(source))
}

// Tier state lives in a JSON file beside the cache rather than in the database.
//
// The cache is disposable and re-derivable, and tying its bookkeeping to the
// master database would mean a cache on a removable disk could desynchronize
// from it. Keeping the manifest with the data means the two travel together.
func (m *Manager) manifestPath() string { return filepath.Join(m.root, "tier-manifest.json") }

func (m *Manager) entries() ([]Entry, error) {
	data, err := os.ReadFile(m.manifestPath())
	if os.IsNotExist(err) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tier: reading manifest: %w", err)
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("tier: parsing manifest: %w", err)
	}

	// Enrich with names from the index, which may have changed since staging.
	for i := range entries {
		if rec, err := m.st.GetModelRecord(entries[i].SHA256); err == nil && rec != nil {
			entries[i].Name = rec.Name
		}
	}
	return entries, nil
}

func (m *Manager) writeEntries(entries []Entry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.manifestPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("tier: writing manifest: %w", err)
	}
	return os.Rename(tmp, m.manifestPath())
}

func (m *Manager) entry(sha string) (*Entry, error) {
	entries, err := m.entries()
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].SHA256 == sha {
			return &entries[i], nil
		}
	}
	return nil, nil
}

func (m *Manager) record(sha, source, cachePath string, size int64, pinned bool) (*Entry, error) {
	now := timestamp.Now()
	entry := Entry{
		SHA256: sha, SourcePath: source, CachePath: cachePath,
		Bytes: size, Pinned: pinned, StagedAt: now, LastUsed: now,
	}
	if err := m.save(entry); err != nil {
		return nil, err
	}

	// The staged copy is another path on the same hash, so the index knows about
	// it exactly as it knows about any other location -- no parallel concept.
	run, err := m.st.BeginScanRun(m.root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(cachePath)
	if err != nil {
		return nil, err
	}
	device, inode := fileIdentity(info)
	if err := m.st.TouchPath(store.FilePath{
		SHA256: sha, Path: cachePath, Root: m.root,
		Device: device, Inode: inode, Size: size,
		MtimeNs: info.ModTime().UnixNano(), Present: true, ScanRunID: run,
	}); err != nil {
		return nil, err
	}
	if err := m.st.FinishScanRun(run, store.StatusCompleted,
		store.ScanCounters{FilesSeen: 1, FilesCached: 1}); err != nil {
		return nil, err
	}
	return &entry, nil
}

func (m *Manager) save(entry Entry) error {
	entries, err := m.entries()
	if err != nil {
		return err
	}
	replaced := false
	for i := range entries {
		if entries[i].SHA256 == entry.SHA256 {
			entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	return m.writeEntries(entries)
}

func (m *Manager) forget(sha string) error {
	entries, err := m.entries()
	if err != nil {
		return err
	}
	out := entries[:0]
	var cachePath string
	for _, e := range entries {
		if e.SHA256 == sha {
			cachePath = e.CachePath
			continue
		}
		out = append(out, e)
	}
	if err := m.writeEntries(out); err != nil {
		return err
	}
	if cachePath != "" {
		// The path is gone from disk, so the index should say so rather than
		// keep claiming a copy that is not there.
		_, _ = m.st.DB().Exec(
			`UPDATE model_file_path SET present = 0 WHERE path = ?`, cachePath)
	}
	return nil
}

func (m *Manager) touch(sha string) error {
	entry, err := m.entry(sha)
	if err != nil || entry == nil {
		return err
	}
	entry.LastUsed = timestamp.Now()
	return m.save(*entry)
}

// copyWithProgress streams a file, reporting progress and honouring cancellation.
func copyWithProgress(ctx context.Context, src, dst string, progress func(copied, total int64)) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("tier: opening source: %w", err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	total := info.Size()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("tier: creating staged copy: %w", err)
	}
	defer out.Close()

	buf := make([]byte, 4<<20)
	var copied int64
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("tier: writing staged copy: %w", writeErr)
			}
			copied += int64(n)
			if progress != nil {
				progress(copied, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("tier: reading source: %w", readErr)
		}
	}
	return out.Sync()
}

func isUnder(child, parent string) bool {
	if child == parent {
		return true
	}
	sep := string(filepath.Separator)
	if !hasSuffix(parent, sep) {
		parent += sep
	}
	return len(child) > len(parent) && child[:len(parent)] == parent
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 5; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Summary renders the status for the CLI.
func (s *Status) Summary() string {
	out := fmt.Sprintf("tier cache %s\n  %d model(s), %s staged",
		s.Root, len(s.Entries), humanBytes(s.UsedBytes))
	if s.CapacityGiB > 0 {
		pct := 100 * float64(s.UsedBytes) / (s.CapacityGiB * float64(1<<30))
		out += fmt.Sprintf(" of %.1f GiB (%.0f%%)", s.CapacityGiB, pct)
	}
	if s.Pinned > 0 {
		out += fmt.Sprintf("\n  %d pinned", s.Pinned)
	}
	return out
}

// SharedExtentsNote explains why reflinks are irrelevant here, since a user who
// just learned about them will reasonably ask.
const SharedExtentsNote = "Reflinks do not help when staging: crossing devices is a genuine copy."
