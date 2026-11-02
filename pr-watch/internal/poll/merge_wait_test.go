package poll

import (
	"slices"
	"strings"
	"testing"
)

// A final note nobody could take yet is kept: the watch stays on until the owner or parent is ready.
func TestPollGoneOwnerBusyParentAtMergeWaits(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	delete(c.terms, "t2")
	c.terms["t1"] = term("t1", 11, "working")
	gh.prs[prURL] = merged(oldComment)
	run(t, d)
	if got := c.to("t1"); len(got) != 0 {
		t.Errorf("busy parent got %q", got)
	}
	if got := load(t, d); !got.Watched || got.Last.WorktreeNoted || len(c.runs) != 0 {
		t.Fatalf("after busy pass: watched %v noted %v runs %v", got.Watched, got.Last.WorktreeNoted, c.runs)
	}
	c.terms["t1"] = term("t1", 11, "idle")
	run(t, d)
	if got := c.to("t1"); !slices.Equal(got, []string{finalParent + "\n" + worktreeLine}) {
		t.Errorf("parent got %q", got)
	}
	if got := load(t, d); got.Watched || !got.Last.WorktreeNoted {
		t.Errorf("entry = %+v", got)
	}
}

func TestPollBusyOwnerNoParentAtMergeWaits(t *testing.T) {
	e := watched(t)
	e.Owner.Parent, e.Owner.ParentCreatedMs = "", 0
	d, c, gh := setup(t, e)
	c.terms["t2"] = term("t2", 12, "working")
	gh.prs[prURL] = merged(oldComment)
	run(t, d)
	if got := c.to("t2"); len(got) != 0 || !load(t, d).Watched {
		t.Fatalf("busy owner got %q, watched %v", got, load(t, d).Watched)
	}
	c.terms["t2"] = term("t2", 12, "idle")
	run(t, d)
	if got := c.to("t2"); len(got) != 1 || !strings.HasPrefix(got[0], finalMerged) {
		t.Errorf("owner got %q", got)
	}
	if load(t, d).Watched {
		t.Error("watch not ended")
	}
}

// With no owner and no parent left, nobody can ever be told, so the watch ends.
func TestPollGoneOwnerGoneParentAtMergeEnds(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	delete(c.terms, "t2")
	delete(c.terms, "t1")
	gh.prs[prURL] = merged(oldComment)
	run(t, d)
	if got := load(t, d); got.Watched || len(c.runs) != 0 {
		t.Errorf("watched %v runs %v", got.Watched, c.runs)
	}
}
