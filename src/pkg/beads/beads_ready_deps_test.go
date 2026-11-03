package beads

import "testing"

func TestReady_DependsOn_BlocksUntilClosed(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	dep, _ := s.Create("dependency", TypeTask, PriorityLow, "worker", "")
	b, _ := s.Create("dependent", TypeTask, PriorityLow, "worker", "")
	if err := s.AddDependency(b.ID, dep.ID); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	if inReady(s.Ready(""), b.ID) {
		t.Fatal("bead with an open dependency must be withheld")
	}
	if !inReady(s.Ready(""), dep.ID) {
		t.Fatal("the dependency itself must remain ready")
	}

	if err := s.Update(dep.ID, func(x *Bead) { x.Status = StatusClosed }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !inReady(s.Ready(""), b.ID) {
		t.Fatal("bead must be released once its dependency is closed")
	}
}

func TestReady_DependsOn_UnknownDependencyFailsClosed(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	b, _ := s.Create("dependent", TypeTask, PriorityLow, "worker", "")
	if err := s.AddDependency(b.ID, "no-such-bead"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	if inReady(s.Ready(""), b.ID) {
		t.Fatal("bead with an unknown dependency must be withheld (fail-closed)")
	}
}
