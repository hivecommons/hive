package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const renovateBodyLF = "This PR contains the following updates:\n\n" +
	"♻ **Rebasing**: Whenever PR becomes conflicted, or you tick the rebase/retry checkbox.\n\n" +
	"---\n\n" +
	" - [ ] <!-- rebase-check -->If you want to rebase/retry this PR, check this box\n\n" +
	"---\n\n" +
	"- [ ] an unrelated task list item\n"

// onlyCheckboxChanged asserts after is before with exactly one byte changed:
// the space inside the rebase-check box, now an x.
func onlyCheckboxChanged(t *testing.T, before, after string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("length changed: %d → %d", len(before), len(after))
	}
	diffs := 0
	for i := range before {
		if before[i] != after[i] {
			diffs++
			if before[i] != ' ' || after[i] != 'x' || i < 3 || !strings.HasPrefix(after[i-3:], "- [x] "+RenovateRebaseCheckMarker) {
				t.Fatalf("byte %d changed %q → %q outside the rebase-check box", i, before[i], after[i])
			}
		}
	}
	if diffs != 1 {
		t.Fatalf("%d bytes changed, want exactly 1", diffs)
	}
}

func TestTickRenovateRebaseCheckbox(t *testing.T) {
	crlf := strings.ReplaceAll(renovateBodyLF, "\n", "\r\n")
	for name, body := range map[string]string{"lf": renovateBodyLF, "crlf": crlf, "box on the last line, no newline": "intro\r\n\r\n - [ ] " + RenovateRebaseCheckMarker + "If you want to rebase/retry this PR, check this box"} {
		t.Run(name, func(t *testing.T) {
			got, state := TickRenovateRebaseCheckbox(body)
			if state != RenovateRebaseCheckboxUnticked {
				t.Fatalf("state = %v, want unticked", state)
			}
			onlyCheckboxChanged(t, body, got)
			if strings.Count(got, "\r\n") != strings.Count(body, "\r\n") {
				t.Fatal("line endings changed")
			}
			if again, st := TickRenovateRebaseCheckbox(got); st != RenovateRebaseCheckboxTicked || again != got {
				t.Fatalf("second tick: state %v, changed=%v; want ticked and unchanged", st, again != got)
			}
		})
	}
	for name, body := range map[string]string{
		"ticked":       strings.Replace(renovateBodyLF, "- [ ] "+RenovateRebaseCheckMarker, "- [x] "+RenovateRebaseCheckMarker, 1),
		"ticked upper": strings.Replace(renovateBodyLF, "- [ ] "+RenovateRebaseCheckMarker, "- [X] "+RenovateRebaseCheckMarker, 1),
	} {
		if got, st := TickRenovateRebaseCheckbox(body); st != RenovateRebaseCheckboxTicked || got != body {
			t.Fatalf("%s: state %v changed=%v, want ticked and unchanged", name, st, got != body)
		}
	}
	plain := "Bumps foo from 1 to 2.\n\n- [ ] an unrelated task list item\n"
	if got, st := TickRenovateRebaseCheckbox(plain); st != RenovateRebaseCheckboxAbsent || got != plain {
		t.Fatalf("no checkbox: state %v changed=%v, want absent and unchanged", st, got != plain)
	}
}

func TestRequestRenovateRebasePatchesOnlyTheCheckbox(t *testing.T) {
	body := strings.ReplaceAll(renovateBodyLF, "\n", "\r\n")
	head := "abc123"
	var patched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/pulls/42" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			out, _ := json.Marshal(map[string]any{"number": 42, "body": body, "head": map[string]any{"sha": head}})
			_, _ = w.Write(out)
		case http.MethodPatch:
			raw, _ := io.ReadAll(r.Body)
			var req map[string]any
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Errorf("PATCH body: %v", err)
			}
			if len(req) != 1 {
				t.Errorf("PATCH sends %v, want only the body", req)
			}
			s, _ := req["body"].(string)
			patched = append(patched, s)
			_, _ = w.Write([]byte(`{"number":42}`))
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "acme", []string{"widget"}, slog.Default())

	state, err := c.RequestRenovateRebase(context.Background(), "acme/widget", 42, head)
	if err != nil || state != RenovateRebaseCheckboxUnticked {
		t.Fatalf("RequestRenovateRebase = %v, %v; want unticked, nil", state, err)
	}
	if len(patched) != 1 {
		t.Fatalf("PATCHes = %d, want 1", len(patched))
	}
	onlyCheckboxChanged(t, body, patched[0])

	// The head moved: nothing is written.
	if _, err := c.RequestRenovateRebase(context.Background(), "acme/widget", 42, "def456"); !errors.Is(err, ErrDependencyBotHeadMoved) {
		t.Fatalf("moved head: err = %v, want ErrDependencyBotHeadMoved", err)
	}
	// Already ticked: nothing is written.
	body = patched[0]
	if state, err := c.RequestRenovateRebase(context.Background(), "acme/widget", 42, head); err != nil || state != RenovateRebaseCheckboxTicked {
		t.Fatalf("ticked box: %v, %v; want ticked, nil", state, err)
	}
	if len(patched) != 1 {
		t.Fatalf("PATCHes = %d, want still 1", len(patched))
	}
}
