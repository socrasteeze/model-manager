// Package link materializes a file at a second path without copying its bytes
// where the filesystem allows it.
//
// This is the portability keystone (spec §9). The agreed model is organize by
// views, not by moving bytes: the app owns a directory tree that consuming tools
// point at, and nothing points at real files directly. How a view entry is
// materialized has to be abstracted, because the correct mechanism differs per
// platform and filesystem -- and getting it wrong is what would make the tool
// Linux-only.
package link

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Strategy is a way of making a file appear at a second path.
type Strategy string

const (
	// Reflink is a copy-on-write clone: a real file sharing blocks with the
	// original. Near-zero disk cost, indistinguishable from an ordinary file to
	// every consumer, and works over SMB with no Samba configuration at all.
	// Preferred wherever available.
	Reflink Strategy = "reflink"

	// BlockClone is the Windows equivalent on ReFS and Dev Drives
	// (DUPLICATE_EXTENTS_TO_FILE). Genuinely copy-on-write, unlike a hardlink.
	BlockClone Strategy = "block-clone"

	// Symlink is cheapest and instant, but see the SMB warning in Warnings().
	Symlink Strategy = "symlink"

	// Hardlink is a second name for one inode. NOT copy-on-write: a tool that
	// rewrites a header in place mutates the original through it (§9.3).
	// Opt-in only.
	Hardlink Strategy = "hardlink"

	// Copy is the only option across filesystems, and the safe Windows default.
	Copy Strategy = "copy"
)

// Capability is what a probe found out about one destination directory.
type Capability struct {
	// Dir is the directory that was probed.
	Dir string

	// SameFilesystemAs is the source directory the probe compared against, when
	// one was given. Reflinks only work within one filesystem.
	SameFilesystemAs string

	// Available lists working strategies, best first.
	Available []Strategy

	// Filesystem is the detected filesystem type, where the platform reports one.
	Filesystem string

	// Notes explain what was tried and what it means.
	Notes []string
}

// Best returns the preferred available strategy.
//
// Preference is not a fixed list: it depends on whether a consumer might write
// through the entry, which is what makes a hardlink dangerous (§9.3).
func (c *Capability) Best(allowHardlink bool) Strategy {
	for _, s := range c.Available {
		if s == Hardlink && !allowHardlink {
			continue
		}
		return s
	}
	return Copy
}

// Supports reports whether a strategy is available here.
func (c *Capability) Supports(s Strategy) bool {
	for _, available := range c.Available {
		if available == s {
			return true
		}
	}
	return false
}

// preferenceOrder ranks strategies. Copy-on-write mechanisms come first because
// they cost nothing and diverge safely on write; hardlink sits below plain copy
// because it is the one mechanism that can corrupt the original.
var preferenceOrder = map[Strategy]int{
	Reflink:    0,
	BlockClone: 1,
	Symlink:    2,
	Copy:       3,
	Hardlink:   4,
}

func sortByPreference(strategies []Strategy) {
	sort.SliceStable(strategies, func(i, j int) bool {
		return preferenceOrder[strategies[i]] < preferenceOrder[strategies[j]]
	})
}

// Probe determines empirically what works between srcDir and dstDir.
//
// Empirically, not by inference. §16.2 makes the point that btrfs subvolumes
// report different st_dev values on the same filesystem, so comparing device IDs
// gives a false negative -- the reliable test is to attempt the operation on a
// real file and see whether it succeeds.
func Probe(srcDir, dstDir string) (*Capability, error) {
	cap := &Capability{Dir: dstDir, SameFilesystemAs: srcDir}

	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return nil, fmt.Errorf("link: preparing probe directory: %w", err)
	}
	if srcDir == "" {
		srcDir = dstDir
	}
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return nil, fmt.Errorf("link: preparing probe source: %w", err)
	}

	cap.Filesystem = filesystemType(dstDir)

	probeSrc, err := os.CreateTemp(srcDir, ".mm-probe-src-*")
	if err != nil {
		return nil, fmt.Errorf("link: creating probe file: %w", err)
	}
	srcPath := probeSrc.Name()
	defer os.Remove(srcPath)

	// Enough bytes that a filesystem has something to share, but small enough
	// that the probe is free.
	if _, err := probeSrc.Write(make([]byte, 64*1024)); err != nil {
		probeSrc.Close()
		return nil, fmt.Errorf("link: writing probe file: %w", err)
	}
	probeSrc.Close()

	attempt := func(s Strategy) {
		dst := filepath.Join(dstDir, fmt.Sprintf(".mm-probe-dst-%s", s))
		os.Remove(dst)
		if err := materialize(srcPath, dst, s); err == nil {
			cap.Available = append(cap.Available, s)
			os.Remove(dst)
		} else {
			cap.Notes = append(cap.Notes, fmt.Sprintf("%s unavailable: %v", s, err))
		}
	}

	for _, s := range []Strategy{Reflink, BlockClone, Symlink, Hardlink} {
		attempt(s)
	}
	// A copy always works, provided there is space. It is not probed because a
	// failing probe would mean the disk is full, which is a different problem.
	cap.Available = append(cap.Available, Copy)

	sortByPreference(cap.Available)
	return cap, nil
}

// Materialize creates dst from src using the given strategy.
//
// The source is only ever read. Nothing here modifies, moves, renames or deletes
// the original, which is the fence in §14.
func Materialize(src, dst string, s Strategy) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("link: creating destination directory: %w", err)
	}
	return materialize(src, dst, s)
}

func materialize(src, dst string, s Strategy) error {
	switch s {
	case Reflink:
		return reflink(src, dst)
	case BlockClone:
		return blockClone(src, dst)
	case Symlink:
		return symlink(src, dst)
	case Hardlink:
		return os.Link(src, dst)
	case Copy:
		return copyFile(src, dst)
	}
	return fmt.Errorf("link: unknown strategy %q", s)
}

func symlink(src, dst string) error {
	abs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	return os.Symlink(abs, dst)
}

// copyFile writes to a temporary name and renames into place, so a consuming
// tool never sees a partially written model under its final name.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".mm-incoming"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// Warnings returns the caveats a user needs before choosing a strategy. These
// are §9.2 and §9.3 made visible in-product rather than buried in a spec.
func Warnings(s Strategy) []string {
	switch s {
	case Symlink:
		return []string{
			"If any tool reaches these models over SMB, a symlink farm will likely fail. " +
				"Samba resolves symlinks server-side only under specific settings, and links " +
				"pointing outside the share are disabled by default. Prefer reflinks where " +
				"they are available.",
		}
	case Hardlink:
		return []string{
			"A hardlink is not a copy: it is a second name for the same file. A tool that " +
				"rewrites a model's header in place will modify the ORIGINAL through it. " +
				"Reflinks and ReFS block clones diverge on write; NTFS hardlinks do not.",
			"Because weights_sha256 is stored, such a rewrite is detectable on the next " +
				"verification pass rather than silently orphaning the record — but detection " +
				"is not prevention.",
		}
	case Copy:
		return []string{
			"Real copies use full disk space. This is the safe default where no " +
				"copy-on-write mechanism is available.",
		}
	}
	return nil
}

// Describe renders a capability report for the CLI.
func (c *Capability) Describe() string {
	out := fmt.Sprintf("link strategies for %s", c.Dir)
	if c.Filesystem != "" {
		out += fmt.Sprintf(" (%s)", c.Filesystem)
	}
	out += "\n"

	for _, s := range c.Available {
		marker := "  "
		if s == c.Best(false) {
			marker = "> "
		}
		out += fmt.Sprintf("%s%s\n", marker, s)
	}
	if len(c.Notes) > 0 {
		out += "\nnot available:\n"
		for _, n := range c.Notes {
			out += "  " + n + "\n"
		}
	}
	if w := Warnings(c.Best(false)); len(w) > 0 {
		out += "\n"
		for _, warning := range w {
			out += "note: " + warning + "\n"
		}
	}
	return out
}
