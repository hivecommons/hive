package prgate

import (
	"context"
	"errors"
	"testing"
)

// v5RequiredContexts is the required-context set recorded from the v5 branch
// protection of hivecommons/hive. Everything but `verify PR contents` must be
// impossible for the gate to post.
var v5RequiredContexts = []string{
	"gate", "DCO", "build-and-test", "test", "coverage", "golangci-lint",
	"gosec (insecure patterns)", "govulncheck (known vulnerabilities)",
	"changelog-fragment-guard", "verify PR contents", "Sign-off survives the squash",
}

func TestPostableContextIsAClosedAllowlist(t *testing.T) {
	allowed := map[string]bool{
		"verify PR contents": true, "PR Type": true, "PR Issue": true, "verify PR contents (hive)": true,
	}
	cases := append([]string{
		"dco", "DCO", "Dco", "DCO ", " DCO", "verify pr contents", "Verify PR Contents", "verify PR contents ",
		"verify PR contents (hive) ", "PR type", "pr issue", "prow/lgtm", "tide", "", " ", "verify PR contents (shadow)",
		"verify PR contents​", "PR Type\n",
	}, v5RequiredContexts...)
	cases = append(cases, AllowedContexts()...)
	for _, name := range cases {
		err := PostableContext(name)
		if allowed[name] {
			if err != nil {
				t.Errorf("PostableContext(%q) = %v, want allowed", name, err)
			}
			continue
		}
		if !errors.Is(err, ErrContextNotAllowed) {
			t.Errorf("PostableContext(%q) = %v, want ErrContextNotAllowed", name, err)
		}
	}
	if len(AllowedContexts()) != len(allowed) {
		t.Errorf("AllowedContexts has %d entries, want %d", len(AllowedContexts()), len(allowed))
	}
}

// recordingAPI records every call a Gate makes.
type recordingAPI struct {
	subject  Subject
	readErr  error
	postErr  error
	posts    []Status
	postHead []string
	reads    int
}

func (r *recordingAPI) ReadSubject(context.Context, string, int) (Subject, error) {
	r.reads++
	return r.subject, r.readErr
}

func (r *recordingAPI) PostStatus(_ context.Context, _ string, _ int, sha, statusContext, state, description string) error {
	r.posts = append(r.posts, Status{statusContext, state, description})
	r.postHead = append(r.postHead, sha)
	return r.postErr
}

// No other context can ever reach the API: the poster refuses it before the
// write, for DCO and for every required context on v5.
func TestPosterNeverWritesAnOtherContext(t *testing.T) {
	for _, name := range append([]string{"dco", "DCO"}, v5RequiredContexts...) {
		if name == ContextVerify {
			continue
		}
		api := &recordingAPI{}
		g := New(api, func(string) Mode { return ModeShadow }, nil)
		err := g.post(context.Background(), "o/r", 1, headA, Status{Context: name, State: StateSuccess})
		if !errors.Is(err, ErrContextNotAllowed) {
			t.Errorf("post(%q) = %v, want ErrContextNotAllowed", name, err)
		}
		if len(api.posts) != 0 {
			t.Errorf("post(%q) reached the API: %+v", name, api.posts)
		}
	}
}

// Every status any mode plans for any title is allowlisted.
func TestPlanOnlyYieldsAllowlistedContexts(t *testing.T) {
	for _, mode := range []Mode{ModeOff, ModeShadow, ModeEnforce} {
		for _, title := range []string{"✨ ok", "bad title", "🐛 fix #12", ""} {
			for _, st := range Plan(mode, title) {
				if err := PostableContext(st.Context); err != nil {
					t.Errorf("Plan(%s, %q) planned %q: %v", mode, title, st.Context, err)
				}
				if st.State != StateSuccess && st.State != StateFailure {
					t.Errorf("Plan(%s, %q) planned state %q", mode, title, st.State)
				}
				if len([]rune(st.Description)) > StatusDescriptionMax {
					t.Errorf("description over the GitHub cap: %q", st.Description)
				}
			}
		}
	}
}
