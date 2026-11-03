package dashboard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

func TestStatusRetainsBootSafetyWarnings(t *testing.T) {
	t.Setenv(config.BootSafetyWarningsEnv, `["safety gate dropped: review.require_labels"]`)
	// A newly loaded config has no knowledge of the overwritten prior snapshot.
	// The boot diagnostic must survive such reloads in the status JSON.
	for i := 0; i < 2; i++ {
		cfg := &config.Config{}
		gov := governor.New(cfg.Governor, cfg.Agents, nil)
		payload := BuildFrontendStatus(gov.GetState(), nil, nil, cfg, nil, gov, nil, nil, nil, nil)
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"configSafetyWarnings":["safety gate dropped: review.require_labels"]`) {
			t.Fatalf("boot safety warning missing from status: %s", body)
		}
	}
}
