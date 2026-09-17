package dashboard

import "testing"

// Regression for hivecommons/hive#7446. Clearing on_demand from the settings
// dialog changed config and nothing else: the agent had been skipped at launch
// BECAUSE it was on-demand, and ReconcileAgents reports only newly ADDED
// agents — never an existing agent whose flag changed. So it had no process, no
// pane, and no tmux session ("no tmux socket found for session hive-reviewer"),
// and stayed that way until the pod was restarted.
//
// The reverse was broken too: an agent that BECAME on-demand kept running while
// the governor stopped kicking it, stranding a live CLI that could never be
// scheduled again.

func TestOnDemandTransition(t *testing.T) {
	for _, tc := range []struct {
		name                string
		prev, next, enabled bool
		want                onDemandAction
		why                 string
	}{
		{
			name: "leaving on-demand starts the agent",
			prev: true, next: false, enabled: true,
			want: onDemandStart,
			why:  "nothing else starts it; it was skipped at launch for being on-demand",
		},
		{
			name: "becoming on-demand stops the agent",
			prev: false, next: true, enabled: true,
			want: onDemandStop,
			why:  "the governor stops kicking it, so a running process would be stranded",
		},
		{
			name: "leaving on-demand while disabled stays down",
			prev: true, next: false, enabled: false,
			want: onDemandNoop,
			why:  "enabling a disabled agent is a separate decision the operator has not made",
		},
		{
			name: "becoming on-demand while disabled is still a stop",
			prev: false, next: true, enabled: false,
			want: onDemandStop,
			why:  "stopping an agent that is not running is a harmless no-op",
		},
		{
			name: "re-saving on-demand does nothing",
			prev: true, next: true, enabled: true,
			want: onDemandNoop,
			why:  "editing an unrelated field must not kill the agent as a side effect",
		},
		{
			name: "re-saving not-on-demand does nothing",
			prev: false, next: false, enabled: true,
			want: onDemandNoop,
			why:  "editing an unrelated field must not restart the agent as a side effect",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := onDemandTransition(tc.prev, tc.next, tc.enabled); got != tc.want {
				t.Errorf("onDemandTransition(prev=%v, next=%v, enabled=%v) = %v, want %v — %s",
					tc.prev, tc.next, tc.enabled, got, tc.want, tc.why)
			}
		})
	}
}

// The settings dialog PUTs only the keys the operator actually changed, so the
// overwhelmingly common save carries no on_demand at all. That path resolves to
// prev == next, and must never touch the process.
func TestOnDemandTransition_UntouchedFlagNeverMovesTheProcess(t *testing.T) {
	for _, flag := range []bool{true, false} {
		for _, enabled := range []bool{true, false} {
			if got := onDemandTransition(flag, flag, enabled); got != onDemandNoop {
				t.Errorf("a save that did not change on_demand (=%v, enabled=%v) returned %v, want %v",
					flag, enabled, got, onDemandNoop)
			}
		}
	}
}
