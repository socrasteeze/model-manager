//go:build !linux && !darwin && !windows

package link

import "fmt"

func reflink(src, dst string) error {
	return fmt.Errorf("no copy-on-write clone is available on this platform")
}

func blockClone(src, dst string) error {
	return fmt.Errorf("block cloning is a Windows ReFS feature")
}

func filesystemType(path string) string { return "" }
