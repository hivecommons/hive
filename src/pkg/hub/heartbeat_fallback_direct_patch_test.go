package hub

import (
	"encoding/json"
	"testing"
	"time"
)

func TestShouldPatchHeartbeatFallbackDirectly(t *testing.T) {
	now := time.Date(2026, 10, 13, 19, 0, 0, 0, time.UTC)
	oldStart := now.Add(-heartbeatFallbackDirectPatchAfter - time.Minute)
	recentStart := now.Add(-heartbeatFallbackDirectPatchAfter + time.Minute)

	tests := []struct {
		name string
		in   heartbeatFallbackDirectPatchInput
		want bool
	}{
		{
			name: "patches exhausted hub-managed reachable fallback",
			in: heartbeatFallbackDirectPatchInput{
				HubManaged: true, KubectlReachable: true,
				Target: "3910932", Current: "d4ce4ea",
				UpgradeStarted: oldStart, Now: now,
			},
			want: true,
		},
		{
			name: "waits for exhaustion window",
			in: heartbeatFallbackDirectPatchInput{
				HubManaged: true, KubectlReachable: true,
				Target: "3910932", Current: "d4ce4ea",
				UpgradeStarted: recentStart, Now: now,
			},
		},
		{
			name: "respects spoke upgrade pause",
			in: heartbeatFallbackDirectPatchInput{
				HubManaged: true, Paused: true, KubectlReachable: true,
				Target: "3910932", Current: "d4ce4ea",
				UpgradeStarted: oldStart, Now: now,
			},
		},
		{
			name: "respects digest pin",
			in: heartbeatFallbackDirectPatchInput{
				HubManaged: true, DigestPinned: true, KubectlReachable: true,
				Target: "3910932", Current: "d4ce4ea",
				UpgradeStarted: oldStart, Now: now,
			},
		},
		{
			name: "skips spoke-managed hives",
			in: heartbeatFallbackDirectPatchInput{
				KubectlReachable: true,
				Target:           "3910932", Current: "d4ce4ea",
				UpgradeStarted: oldStart, Now: now,
			},
		},
		{
			name: "skips unreachable clusters",
			in: heartbeatFallbackDirectPatchInput{
				HubManaged: true,
				Target:     "3910932", Current: "d4ce4ea",
				UpgradeStarted: oldStart, Now: now,
			},
		},
		{
			name: "throttles repeat direct patch for same target",
			in: heartbeatFallbackDirectPatchInput{
				HubManaged: true, KubectlReachable: true,
				Target: "3910932", Current: "d4ce4ea",
				UpgradeStarted: oldStart, Now: now,
				LastPatch: heartbeatFallbackDirectPatchRecord{
					Target: "3910932",
					At:     now.Add(-heartbeatFallbackDirectPatchCooldown + time.Minute),
				},
			},
		},
		{
			name: "new target bypasses old target throttle",
			in: heartbeatFallbackDirectPatchInput{
				HubManaged: true, KubectlReachable: true,
				Target: "3910932", Current: "d4ce4ea",
				UpgradeStarted: oldStart, Now: now,
				LastPatch: heartbeatFallbackDirectPatchRecord{
					Target: "aaaaaaaa",
					At:     now.Add(-time.Minute),
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := shouldPatchHeartbeatFallbackDirectly(tt.in)
			if got != tt.want {
				t.Fatalf("shouldPatchHeartbeatFallbackDirectly() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHostedDeploymentImagePatchIncludesInitContainers(t *testing.T) {
	at := time.Date(2026, 10, 13, 19, 30, 0, 0, time.UTC)
	patch, err := hostedDeploymentImagePatch([]byte(`{
		"spec": {
			"template": {
				"spec": {
					"initContainers": [
						{"name": "copy-config"},
						{"name": "init-permissions"}
					],
					"containers": [
						{"name": "hive"}
					]
				}
			}
		}
	}`), "ghcr.io/hivecommons/hive:3910932", at)
	if err != nil {
		t.Fatalf("hostedDeploymentImagePatch() error = %v", err)
	}
	var got struct {
		Spec struct {
			Template struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
				Spec struct {
					Containers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"containers"`
					InitContainers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"initContainers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(patch), &got); err != nil {
		t.Fatalf("patch is not JSON: %v", err)
	}
	if got.Spec.Template.Metadata.Annotations[restartAtAnnotation] != at.Format(time.RFC3339) {
		t.Fatalf("restart annotation = %q, want %q",
			got.Spec.Template.Metadata.Annotations[restartAtAnnotation], at.Format(time.RFC3339))
	}
	if len(got.Spec.Template.Spec.Containers) != 1 ||
		got.Spec.Template.Spec.Containers[0].Name != "hive" ||
		got.Spec.Template.Spec.Containers[0].Image != "ghcr.io/hivecommons/hive:3910932" {
		t.Fatalf("containers patch = %+v", got.Spec.Template.Spec.Containers)
	}
	if len(got.Spec.Template.Spec.InitContainers) != 2 {
		t.Fatalf("initContainers patch length = %d, want 2", len(got.Spec.Template.Spec.InitContainers))
	}
	for _, c := range got.Spec.Template.Spec.InitContainers {
		if c.Image != "ghcr.io/hivecommons/hive:3910932" {
			t.Fatalf("init container %s image = %q", c.Name, c.Image)
		}
	}
}
