//go:build linux

package store

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// classifyMount finds the mount covering path by walking /proc/self/mountinfo and
// selecting the entry with the longest matching mount point. That longest-prefix
// rule matters: a bind mount or a nested share under an otherwise-local tree is
// exactly the case a naive "check the first match" implementation gets wrong.
func classifyMount(path string) (MountKind, string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return MountUnknown, ""
	}
	// The database file itself may not exist yet; classify the deepest existing
	// ancestor, which is the directory it will be created in.
	abs = deepestExisting(abs)

	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return MountUnknown, ""
	}
	defer f.Close()

	bestLen := -1
	bestType := ""

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// mountinfo format:
		//   id parent major:minor root mountpoint opts [optional...] - fstype source superopts
		line := sc.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+3:])
		if len(left) < 5 || len(right) < 1 {
			continue
		}
		mountPoint := unescapeMountField(left[4])
		fstype := right[0]

		if !pathUnder(abs, mountPoint) {
			continue
		}
		if len(mountPoint) > bestLen {
			bestLen = len(mountPoint)
			bestType = fstype
		}
	}

	if bestLen < 0 {
		return MountUnknown, ""
	}
	if isNetworkFSType(bestType) {
		return MountNetwork, bestType
	}
	return MountLocal, bestType
}

// pathUnder reports whether abs is at or below mountPoint.
func pathUnder(abs, mountPoint string) bool {
	if mountPoint == "/" {
		return true
	}
	if abs == mountPoint {
		return true
	}
	return strings.HasPrefix(abs, mountPoint+"/")
}

// deepestExisting returns the closest ancestor of abs that exists, so a
// not-yet-created database file still gets classified by the directory it will
// land in.
func deepestExisting(abs string) string {
	for {
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return abs
		}
		abs = parent
	}
}

// unescapeMountField decodes the octal escapes the kernel uses in mountinfo for
// space, tab, newline and backslash.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			ok := true
			for j := 1; j <= 3; j++ {
				c := s[i+j]
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
