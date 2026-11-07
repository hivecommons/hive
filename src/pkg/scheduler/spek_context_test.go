package scheduler

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The Spek hand-off (spektacular#114): a Spek-published child kicked to a
// fixer carries the RFC link and spek identity in its kick context, so the
// agent builds against the spek's acceptance criteria and links its PR to
// the child with `Fixes #N`.

func newSpekContextScheduler(t *testing.T) *Scheduler {
	t.Helper()
	cfg := &config.Config{Project: config.ProjectConfig{Org: "myorg", Repos: []string{"myorg/app"}}}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

func spekChildIssue(body string) github.Issue {
	return github.Issue{Repo: "myorg/app", Number: 42, Title: "billing: retry failed charges", Body: body, URL: "https://github.com/myorg/app/issues/42"}
}

func TestSpekPublishedContextInKickList(t *testing.T) {
	s := newSpekContextScheduler(t)
	issue := spekChildIssue("Retry failed charges.\n<!-- spek:publish name=billing-retries artifact_id=spk_01 target=github task=t3 rfc=https://github.com/myorg/app/issues/40 -->")
	out, _ := s.formatIssueListWithPolicy([]github.Issue{issue})
	for _, want := range []string{
		"specified by Spek:",
		"RFC https://github.com/myorg/app/issues/40",
		"spek billing-retries, artifact spk_01, task t3",
		"`Fixes #42`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("kick list missing %q:\n%s", want, out)
		}
	}
}

func TestSpekPublishedContextAbsentWithoutMarker(t *testing.T) {
	s := newSpekContextScheduler(t)
	for name, body := range map[string]string{
		"plain body": "an ordinary issue body",
		"malformed":  "<!-- spek:publish name=only -->",
	} {
		t.Run(name, func(t *testing.T) {
			out, _ := s.formatIssueListWithPolicy([]github.Issue{spekChildIssue(body)})
			if strings.Contains(out, "specified by Spek") {
				t.Errorf("unexpected Spek block:\n%s", out)
			}
		})
	}
}

func TestSpekPublishedContextOmitsUntrustedRFC(t *testing.T) {
	line := spekPublishedContext(spekChildIssue("<!-- spek:publish name=n artifact_id=a target=github task=t1 rfc=javascript:alert(1) -->"))
	if line == "" || strings.Contains(line, "RFC ") || strings.Contains(line, "javascript") {
		t.Errorf("untrusted rfc= must be dropped, block kept: %q", line)
	}
}
