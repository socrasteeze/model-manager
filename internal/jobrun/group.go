package jobrun

// Serializing the jobs that share one external rate limit.
//
// Runner enforces at-most-one within a Runner, which is the wrong unit here.
// Enrichment, update sweeps and archive intake are separate Runners spending one
// origin.Client throttle, and at-most-one-each is not the constraint that
// matters: run together they are each politely paced and jointly multiply the
// request rate against the API the throttle exists to placate -- which is how a
// sweep earns the rate limit that then stops all of them.
//
// This was a pairwise check in the API layer, and it was already wrong: the
// update sweep asked whether enrichment was running, and enrichment did not ask
// about updates. Two members need two checks, three need six, four need twelve,
// and the one that gets forgotten is silent.

import (
	"sort"
	"sync"
)

// Group serializes members that share one external rate limit.
//
// Members register as closures rather than as values, so this package does not
// import the job packages that import it.
type Group struct {
	mu      sync.Mutex
	members []member
}

type member struct {
	key      string
	label    string
	inFlight func() (string, bool)
}

// Add registers a member.
//
// key identifies it to Busy's caller so a runner is never told it conflicts with
// itself. label is what a user reads in the refusal.
func (g *Group) Add(key, label string, inFlight func() (string, bool)) {
	if g == nil || inFlight == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, m := range g.members {
		if m.key == key {
			g.members[i] = member{key: key, label: label, inFlight: inFlight}
			return
		}
	}
	g.members = append(g.members, member{key: key, label: label, inFlight: inFlight})
}

// Busy names a running member other than except.
//
// Excluding the caller matters: a runner asking whether it may start would
// otherwise find itself and refuse. Runner already answers that case properly,
// and its refusal carries the running job so a client that lost track of it can
// re-attach -- a Group conflict cannot do that, so it must not shadow it.
func (g *Group) Busy(except string) (label, id string, busy bool) {
	if g == nil {
		return "", "", false
	}
	g.mu.Lock()
	members := append([]member(nil), g.members...)
	g.mu.Unlock()

	for _, m := range members {
		if m.key == except {
			continue
		}
		if jobID, running := m.inFlight(); running {
			return m.label, jobID, true
		}
	}
	return "", "", false
}

// Keys lists the registered members, sorted.
//
// Exists so a test can iterate the real membership rather than a list of its
// own: a fourth member added without a matching test is exactly the omission
// this type was built to stop being silent.
func (g *Group) Keys() []string {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.members))
	for _, m := range g.members {
		out = append(out, m.key)
	}
	sort.Strings(out)
	return out
}
