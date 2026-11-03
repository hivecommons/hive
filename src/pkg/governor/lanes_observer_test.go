package governor

import "testing"

func TestAdditionalModeObserverPreservesHooksAndLockBoundary(t *testing.T) {
	g := observerTestGovernor(t)
	order := []string{}
	g.SetModeChangeObserver(func(ModeChange) { order = append(order, "hooks") })
	g.AddModeChangeObserver(nil)
	g.AddModeChangeObserver(func(c ModeChange) {
		if g.GetState().Mode != c.To {
			t.Fatal("observer did not see committed state")
		}
		order = append(order, "lanes")
	})
	g.Evaluate(20, 0, 0, 0)
	if len(order) != 2 || order[0] != "hooks" || order[1] != "lanes" {
		t.Fatalf("observer order: %v", order)
	}
	g.SetModeChangeObserver(nil)
	g.AddModeChangeObserver(func(ModeChange) { order = append(order, "new") })
	g.Evaluate(0, 0, 0, 0)
	if order[len(order)-1] != "new" {
		t.Fatal("observer not installed without predecessor")
	}
}
