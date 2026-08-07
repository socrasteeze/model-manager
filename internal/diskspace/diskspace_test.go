package diskspace

import (
	"errors"
	"testing"
)

// Avail must answer for a directory that really exists, on whatever platform
// the suite is running on. The number is unknowable, but "positive" and "did
// not error" are the two properties every caller depends on.
func TestAvailAnswersForARealDirectory(t *testing.T) {
	got, err := Avail(t.TempDir())
	if errors.Is(err, ErrUnsupported) {
		t.Skip("no implementation on this platform, which is itself a supported outcome")
	}
	if err != nil {
		t.Fatalf("Avail: %v", err)
	}
	if got <= 0 {
		t.Errorf("Avail = %d; a writable temp directory reporting no space would refuse every download", got)
	}
}

// A directory that does not exist must error rather than answer 0. Answering 0
// would read as "full" and refuse a download for a destination the caller is
// about to create -- which is why callers resolve to an existing ancestor first.
func TestAvailRefusesAMissingDirectory(t *testing.T) {
	if _, err := Avail(t.TempDir() + "/does/not/exist"); err == nil {
		t.Fatal("a missing directory answered instead of erroring")
	}
}

// The margin is the larger of a floor and a percentage, and the crossover is the
// interesting part: below it the floor governs, above it the percentage does.
func TestMarginTakesTheLargerOfFloorAndPercentage(t *testing.T) {
	const floor = 512 << 20 // 512 MiB
	const crossover = floor * 20

	cases := []struct {
		name string
		size int64
		want int64
	}{
		{"a small LoRA gets the floor", 200 << 20, floor},
		{"zero gets the floor", 0, floor},
		{"exactly at the crossover still gets the floor", crossover, floor},
		{"just past the crossover switches to 5%", crossover + 20, (crossover + 20) / 20},
		{"a 40 GB checkpoint gets 2 GB", 40 << 30, (40 << 30) / 20},
	}
	for _, tc := range cases {
		if got := Margin(tc.size); got != tc.want {
			t.Errorf("%s: Margin(%d) = %d, want %d", tc.name, tc.size, got, tc.want)
		}
	}

	// The property the cases above are instances of: never below the floor, and
	// never below 5%, for any size.
	for _, size := range []int64{1, 1 << 20, 1 << 30, 1 << 40, 1 << 45} {
		m := Margin(size)
		if m < floor {
			t.Errorf("Margin(%d) = %d, below the floor", size, m)
		}
		if m < size/20 {
			t.Errorf("Margin(%d) = %d, below 5%%", size, m)
		}
	}
}
