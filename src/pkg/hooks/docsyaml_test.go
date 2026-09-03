package hooks

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"gopkg.in/yaml.v3"
)

// Every hooks: block appearing in src/docs/hooks.md must actually validate.
func TestDocExamplesValidate(t *testing.T) {
	examples := []string{
		`hooks:
  - name: review-rejected-notify
    on: review_rejected
    action: notify
    params:
      priority: high`,
		`hooks:
  - name: pause-reviewer-on-repeated-rejects
    on: review_rejected
    action: pause
    when: t.agent == "reviewer" && t.pin != ""
    params:
      reason: repeated low-quality output on a stale pin`,
		`hooks:
  - name: surge-alert
    on: governor_mode_change
    action: notify
    when: t.to == "surge"
    params:
      priority: high
      title: Governor entered surge`,
		`hooks:
  - name: record-upgrade-pause
    on: upgrade_pause
    action: annotate
    params:
      note: upgrade delivery kill switch flipped`,
		`hooks:
  - name: gate-high-autonomy
    on: acmm_level_change
    action: enqueue-approval
    when: t.to == "5" || t.to == "6"
    params:
      kind: acmm-raise
      summary: Confirm raising fleet autonomy to L5+`,
		`hooks:
  - name: sweep-summary
    on: sweep_completed
    action: notify
    rate_limit_per_minute: 2
    params:
      priority: low`,
		`hooks:
  - name: attr-example
    on: review_rejected
    action: notify
    when: attr(t.attrs, "pr") != ""`,
	}
	for i, ex := range examples {
		var cfg config.Config
		if err := yaml.Unmarshal([]byte(ex), &cfg); err != nil {
			t.Fatalf("example %d: yaml: %v", i, err)
		}
		if len(cfg.Hooks) == 0 {
			t.Fatalf("example %d: parsed no hooks (yaml tag mismatch?)", i)
		}
		if _, err := CompileFromConfig(&cfg); err != nil {
			t.Errorf("example %d (%s): docs example does not validate: %v", i, cfg.Hooks[0].Name, err)
		}
	}
}
