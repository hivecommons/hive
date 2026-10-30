package hub

import (
	"testing"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// #15788: a hub-managed hive whose latch is open onto a build it is not
// running gets told in the heartbeat response that its pod is being rolled,
// so its dashboard animates the roll before the SSE stream drops.
func TestHeartbeatUpgradeAnnounce(t *testing.T) {
	const target = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name string
		in   heartbeatUpgradeAnnounceInput
		want *spoke.UpgradeAnnounce
	}{
		{"hub-managed open latch", heartbeatUpgradeAnnounceInput{HubManaged: true, LatchOpen: true, Target: target, Current: "deadbeef"},
			&spoke.UpgradeAnnounce{Target: target, Source: spoke.UpgradeSourceHub, Reason: "hub-managed upgrade"}},
		{"already on target", heartbeatUpgradeAnnounceInput{HubManaged: true, LatchOpen: true, Target: target, Current: "0123456"}, nil},
		{"channel target announces", heartbeatUpgradeAnnounceInput{HubManaged: true, LatchOpen: true, Target: "stable", Current: "deadbeef"},
			&spoke.UpgradeAnnounce{Target: "stable", Source: spoke.UpgradeSourceHub, Reason: "hub-managed upgrade"}},
		{"spoke-managed: it drives its own drain", heartbeatUpgradeAnnounceInput{HubManaged: false, LatchOpen: true, Target: target, Current: "deadbeef"}, nil},
		{"spoke already reports upgrading", heartbeatUpgradeAnnounceInput{HubManaged: true, LatchOpen: true, Target: target, Current: "deadbeef", SpokeReportedUpgrading: true}, nil},
		{"latch closed", heartbeatUpgradeAnnounceInput{HubManaged: true, Target: target, Current: "deadbeef"}, nil},
		{"restart announces without a target", heartbeatUpgradeAnnounceInput{Restart: true},
			&spoke.UpgradeAnnounce{Source: spoke.UpgradeSourceHub, Reason: "hub restart"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := heartbeatUpgradeAnnounce(tc.in)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if got != nil && *got != *tc.want {
				t.Fatalf("got %+v, want %+v", *got, *tc.want)
			}
		})
	}
}
