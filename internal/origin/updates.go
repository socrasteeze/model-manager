package origin

// Update-check statistics.
//
// This file used to hold CheckUpdates as well: an in-memory sweep that built a
// LocalIndex, asked each owned model for its newest version, and returned a
// []Update the caller printed and then discarded. SweepUpdates replaced it,
// because the answer has to survive a restart and be filterable -- so it
// records what the provider said and lets the model_update view decide what
// that means.
//
// The old implementation is deleted rather than kept beside the new one. Two
// answers to "what needs updating", computed different ways, is exactly the
// drift the view's exclusions were written to prevent: they mirror the old
// checker's rules deliberately, and a second live copy of those rules would be
// free to disagree with them.

import (
	"fmt"
	"time"
)

// UpdateStats summarizes a sweep.
type UpdateStats struct {
	Checked int
	Found   int
	Errors  int

	// RateLimited means the provider cut the run short: the result covers only
	// the models checked so far. Without this flag a truncated sweep is
	// indistinguishable from "everything else is up to date", which is a wrong
	// answer presented confidently.
	RateLimited bool

	Elapsed time.Duration
}

// Summary renders the stats for the CLI.
func (s *UpdateStats) Summary() string {
	out := fmt.Sprintf("checked %d  updates %d", s.Checked, s.Found)
	if s.Errors > 0 {
		out += fmt.Sprintf("  errors %d", s.Errors)
	}
	if s.RateLimited {
		out += "  (rate limited — partial result, re-run to continue)"
	}
	return out + fmt.Sprintf("  (%s)", s.Elapsed.Round(time.Second))
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
