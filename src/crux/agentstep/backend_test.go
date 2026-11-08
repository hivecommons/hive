package agentstep

import (
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/sandbox"
)

func TestLookupKnowsTheThreeBackends(t *testing.T) {
	for _, name := range []string{api.StepBackendClaude, api.StepBackendCodex, api.StepBackendProcess} {
		b, err := Lookup(name)
		if err != nil || b.Name() != name {
			t.Fatalf("Lookup(%s) = %v, %v", name, b, err)
		}
	}
	var reqErr *RequestError
	if _, err := Lookup("gemini"); !errors.As(err, &reqErr) || !strings.Contains(err.Error(), "claude") {
		t.Fatalf("Lookup(gemini) = %v", err)
	}
}

func TestProcessBackendCommandAndParse(t *testing.T) {
	spec := Spec{PromptRel: ".crux/steps/1/prompt-1.md", Command: []string{"python3", "ask.py", "it's"}}
	cmd, err := Backends["process"].Command(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sh", "-lc", `'python3' 'ask.py' 'it'\''s' < '.crux/steps/1/prompt-1.md'`}
	if strings.Join(cmd, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Command = %q", cmd)
	}
	var reqErr *RequestError
	var unsupported *UnsupportedError
	if _, err := Backends["process"].Command(Spec{}); !errors.As(err, &reqErr) {
		t.Errorf("no command: %v", err)
	}
	for _, s := range []Spec{
		{Command: []string{"x"}, Tools: []string{"Bash"}},
		{Command: []string{"x"}, TurnLimit: 2},
		{Command: []string{"x"}, Model: "m"},
	} {
		if _, err := Backends["process"].Command(s); !errors.As(err, &unsupported) {
			t.Errorf("%+v: %v", s, err)
		}
	}
	out := Backends["process"].Parse(spec, sandbox.Result{Stdout: "note\n```json\n{\"a\":1}\n```\n"}, nil)
	if out.Status != "" || string(out.Answer) != `{"a":1}` {
		t.Fatalf("Parse fenced = %+v", out)
	}
	out = Backends["process"].Parse(spec, sandbox.Result{ExitCode: 3}, nil)
	if out.Status != api.StepBackendError || !strings.Contains(out.Reason, "exited 3") {
		t.Fatalf("Parse failure = %+v", out)
	}
	if out := Backends["process"].Parse(spec, sandbox.Result{Stdout: "nothing here"}, nil); out.Answer != nil {
		t.Fatalf("Parse prose = %+v", out)
	}
}

func TestClaudeBackendCommandAndParse(t *testing.T) {
	cmd, err := Backends["claude"].Command(Spec{PromptRel: "p.md", Model: "opus", Tools: []string{"Read", "Grep"}, TurnLimit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if line := cmd[2]; !strings.Contains(line, "claude -p --output-format json --model 'opus' --allowedTools 'Read,Grep' --max-turns 3 < 'p.md'") {
		t.Fatalf("Command = %q", line)
	}
	cmd, _ = Backends["claude"].Command(Spec{PromptRel: "p.md"})
	if !strings.Contains(cmd[2], "--dangerously-skip-permissions") || strings.Contains(cmd[2], "--max-turns") {
		t.Fatalf("no-tools command = %q", cmd[2])
	}
	var reqErr *RequestError
	if _, err := Backends["claude"].Command(Spec{Command: []string{"x"}}); !errors.As(err, &reqErr) {
		t.Errorf("command with claude: %v", err)
	}
	for _, tc := range []struct {
		stdout string
		status string
		answer string
	}{
		{`{"type":"result","subtype":"success","result":"Sure: {\"ok\":true}","num_turns":2,"usage":{"input_tokens":10,"output_tokens":5}}`, "", `{"ok":true}`},
		{`{"type":"result","subtype":"success","result":"no json","num_turns":1}`, "", ""},
		{`{"type":"result","subtype":"success","result":"{}","permission_denials":[{"tool_name":"Bash"}]}`, api.StepToolDenied, ""},
		{`{"type":"result","subtype":"error_max_turns","num_turns":3}`, api.StepTurnLimitExceeded, ""},
		{`{"type":"result","subtype":"error_during_execution","is_error":true}`, api.StepBackendError, ""},
		{`garbage`, api.StepBackendError, ""},
	} {
		out := Backends["claude"].Parse(Spec{}, sandbox.Result{Stdout: tc.stdout}, nil)
		if out.Status != tc.status || string(out.Answer) != tc.answer {
			t.Errorf("Parse(%s) = %+v", tc.stdout, out)
		}
		if tc.answer != "" && (out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 5 || out.Turns != 2) {
			t.Errorf("usage = %+v", out)
		}
	}
}

func TestCodexBackendCommandAndParse(t *testing.T) {
	cmd, err := Backends["codex"].Command(Spec{PromptRel: "p.md", AnswerRel: "a.txt", Model: "o3", TurnLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd[2], "codex exec --json --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox --model 'o3' --output-last-message 'a.txt' - < 'p.md'") {
		t.Fatalf("Command = %q", cmd[2])
	}
	var unsupported *UnsupportedError
	if _, err := Backends["codex"].Command(Spec{Tools: []string{"x"}}); !errors.As(err, &unsupported) {
		t.Errorf("tools: %v", err)
	}
	if _, err := Backends["codex"].Command(Spec{TurnLimit: 2}); !errors.As(err, &unsupported) {
		t.Errorf("turn limit: %v", err)
	}
	stdout := strings.Join([]string{
		`{"type":"thread.started"}`,
		`not json`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"{\"from\":\"stdout\"}"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":3}}`,
	}, "\n")
	files := map[string]string{"a.txt": "{\"from\":\"file\"}\n"}
	read := func(rel string) ([]byte, error) {
		if v, ok := files[rel]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("missing")
	}
	spec := Spec{AnswerRel: "a.txt"}
	out := Backends["codex"].Parse(spec, sandbox.Result{Stdout: stdout}, read)
	if out.Status != "" || string(out.Answer) != `{"from":"file"}` || out.Usage.InputTokens != 7 || out.Usage.OutputTokens != 3 {
		t.Fatalf("Parse = %+v", out)
	}
	out = Backends["codex"].Parse(Spec{AnswerRel: "none"}, sandbox.Result{Stdout: stdout}, read)
	if string(out.Answer) != `{"from":"stdout"}` {
		t.Fatalf("Parse without a file = %+v", out)
	}
	out = Backends["codex"].Parse(spec, sandbox.Result{Stdout: `{"type":"error","message":"quota"}`, ExitCode: 1}, read)
	if out.Status != api.StepBackendError || out.Reason != "quota" {
		t.Fatalf("Parse failure = %+v", out)
	}
	out = Backends["codex"].Parse(spec, sandbox.Result{ExitCode: 2}, read)
	if out.Status != api.StepBackendError || !strings.Contains(out.Reason, "exited 2") {
		t.Fatalf("Parse silent failure = %+v", out)
	}
}
