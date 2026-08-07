package jobrun

import "testing"

func stub(id string, running bool) func() (string, bool) {
	return func() (string, bool) { return id, running }
}

// A member must never conflict with itself. Runner already refuses that case
// properly, and its refusal carries the running job so a client that lost track
// of it can re-attach -- a group conflict cannot do that, so it must not shadow
// it with a worse message.
func TestBusyExcludesTheCaller(t *testing.T) {
	g := &Group{}
	g.Add("enrich", "enrichment", stub("enrich-3", true))

	if _, _, busy := g.Busy("enrich"); busy {
		t.Error("a member was told it conflicts with itself")
	}
	label, id, busy := g.Busy("updates")
	if !busy {
		t.Fatal("a running member did not block a different one")
	}
	if label != "enrichment" || id != "enrich-3" {
		t.Errorf("Busy = (%q, %q), want the running member's label and job id", label, id)
	}
}

// The bug this type replaced: the check was pairwise and one-directional, so an
// update sweep refused to start while enrichment ran, and enrichment happily
// started on top of an update sweep. Both directions now hold by construction.
func TestExclusionIsSymmetric(t *testing.T) {
	g := &Group{}
	enrichRunning, updatesRunning := false, false
	g.Add("enrich", "enrichment", func() (string, bool) { return "e1", enrichRunning })
	g.Add("updates", "update check", func() (string, bool) { return "u1", updatesRunning })

	enrichRunning = true
	if _, _, busy := g.Busy("updates"); !busy {
		t.Error("updates was allowed to start while enrichment ran")
	}
	enrichRunning, updatesRunning = false, true
	if _, _, busy := g.Busy("enrich"); !busy {
		t.Error("enrichment was allowed to start while an update sweep ran; this was the original bug")
	}
}

func TestBusyIsFalseWhenNothingRuns(t *testing.T) {
	g := &Group{}
	g.Add("enrich", "enrichment", stub("", false))
	g.Add("updates", "update check", stub("", false))

	if _, _, busy := g.Busy("archive"); busy {
		t.Error("an idle group reported busy")
	}
}

// A nil group is the shape a daemon with no sweeps has -- read-only, or
// --no-remote. It must be usable rather than a nil dereference on every prereq.
func TestNilGroupIsUsable(t *testing.T) {
	var g *Group
	if _, _, busy := g.Busy("anything"); busy {
		t.Error("a nil group reported busy")
	}
	g.Add("enrich", "enrichment", stub("x", true)) // must not panic
	if keys := g.Keys(); keys != nil {
		t.Errorf("Keys on a nil group = %v", keys)
	}
}

// Re-registering replaces rather than duplicating, so a member cannot end up
// listed twice and reported as blocking itself through its own second entry.
func TestAddReplacesAnExistingMember(t *testing.T) {
	g := &Group{}
	g.Add("enrich", "old", stub("a", true))
	g.Add("enrich", "new", stub("b", true))

	if keys := g.Keys(); len(keys) != 1 || keys[0] != "enrich" {
		t.Fatalf("Keys = %v, want one entry", keys)
	}
	if label, id, _ := g.Busy("other"); label != "new" || id != "b" {
		t.Errorf("Busy = (%q, %q), want the replacement", label, id)
	}
}
