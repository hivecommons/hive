package test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var testIdeas = []string{
	"A Go CLI tool that recursively scans file permissions and outputs JSON/Markdown reports",
	"A Python web scraper that extracts product prices from e-commerce sites and tracks changes",
	"A TypeScript React dashboard for monitoring Kubernetes cluster health across namespaces",
	"A Go library for parsing and validating YAML configuration files with schema support",
	"A Python CLI that analyzes Git repositories for security vulnerabilities in dependencies",
	"A TypeScript API server for managing todo lists with real-time WebSocket sync",
	"A Go operator that auto-scales Kubernetes deployments based on custom metrics",
	"A Python data pipeline that processes CSV files and loads them into PostgreSQL",
	"A TypeScript CLI that generates OpenAPI specs from TypeScript interfaces",
	"A Go microservice that proxies HTTP requests with rate limiting and caching",
}

const totalPasses = 10000

func TestInceptionE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e in short mode")
	}

	client := newAPIClient()

	// Verify hive is reachable
	_, code, err := client.get("/api/version")
	if err != nil || code != 200 {
		t.Fatalf("hive not reachable at %s: %v (code=%d)", hiveURL, err, code)
	}

	passed := 0
	failed := 0
	var failures []PassResult

	for pass := 1; pass <= totalPasses; pass++ {
		idea := testIdeas[(pass-1)%len(testIdeas)]
		idea = fmt.Sprintf("%s (pass %d)", idea, pass)

		t.Run(fmt.Sprintf("Pass_%d", pass), func(t *testing.T) {
			start := time.Now()
			result := runSinglePass(t, client, pass, idea)
			result.Duration = time.Since(start).String()

			logResult(t, result)

			if result.Status == "fail" {
				failed++
				failures = append(failures, result)
				t.Errorf("Pass %d FAILED at phase=%s check=%s: %s", pass, result.Phase, result.Check, result.Error)
			} else {
				passed++
			}

			if pass%100 == 0 {
				t.Logf("Progress: %d/%d (passed=%d, failed=%d)", pass, totalPasses, passed, failed)
			}
		})
	}

	t.Logf("FINAL: %d/%d passed, %d failed", passed, totalPasses, failed)
	if failed > 0 {
		t.Logf("Failure summary:")
		for _, f := range failures {
			t.Logf("  Pass %d: [%s] %s at %s — %s", f.Pass, f.Category, f.Check, f.Phase, f.Error)
		}
	}
}

func runSinglePass(t *testing.T, client *apiClient, pass int, idea string) PassResult {
	result := PassResult{
		Pass:      pass,
		Idea:      idea,
		Timestamp: time.Now(),
	}

	// Step 0: Restart brainstorm agent for clean context
	result.Phase = "restart"
	result.Check = "restart_ok"
	client.post("/api/restart/brainstorm", nil)
	time.Sleep(5 * time.Second)

	// Step 1: Reset
	result.Phase = "reset"
	result.Check = "reset_ok"
	data, code, err := client.post("/api/inception/reset", nil)
	if err != nil {
		return fail(result, "api_error", fmt.Sprintf("reset failed: %v", err))
	}
	if code != 200 {
		return fail(result, "api_error", fmt.Sprintf("reset returned %d: %v", code, data))
	}

	// Step 2: Start inception
	result.Phase = "capture"
	result.Check = "start_ok"
	data, code, err = client.post("/api/inception/start", map[string]string{"idea": idea})
	if err != nil {
		return fail(result, "api_error", fmt.Sprintf("start failed: %v", err))
	}
	if code != 200 {
		return fail(result, "api_error", fmt.Sprintf("start returned %d: %v", code, data))
	}

	// Verify state
	result.Check = "start_state"
	state, err := client.inceptionState()
	if err != nil {
		return fail(result, "api_error", fmt.Sprintf("state check failed: %v", err))
	}
	if state == nil {
		return fail(result, "assertion", "state is nil after start")
	}
	if state["phase"] != "capture" {
		return fail(result, "assertion", fmt.Sprintf("expected phase=capture, got %v", state["phase"]))
	}
	if state["idea_text"] == nil || state["idea_text"].(string) == "" {
		return fail(result, "assertion", "idea_text is empty after start")
	}

	// CDP: verify capture phase UI
	if cdpErr := verifyCDPPhase("capture", pass); cdpErr != "" {
		t.Logf("Pass %d CDP warning (capture): %s", pass, cdpErr)
	}

	// Step 2.3: Verify brainstorm agent actually started (not stuck at no output)
	result.Check = "agent_started"
	{
		const agentStartWait = 30 * time.Second
		const agentStartPoll = 5 * time.Second
		deadline := time.Now().Add(agentStartWait)
		started := false
		for time.Now().Before(deadline) {
			lines, _ := client.paneOutput("brainstorm")
			if len(lines) > 2 {
				started = true
				break
			}
			time.Sleep(agentStartPoll)
		}
		if !started {
			return fail(result, "agent_error", "brainstorm agent has no output after 30s — RestartWithBootstrap may have failed")
		}
	}

	// Step 2.5: Verify brainstorm agent is processing the inception idea
	result.Phase = "capture"
	result.Check = "agent_on_task"
	{
		const agentCheckAttempts = 6
		const agentCheckInterval = 10 * time.Second
		agentOnTask := false
		for attempt := 0; attempt < agentCheckAttempts; attempt++ {
			time.Sleep(agentCheckInterval)
			agentLines, _ := client.paneOutput("brainstorm")
			agentText := strings.ToLower(strings.Join(agentLines, " "))
			ideaWords := strings.Fields(strings.ToLower(idea))
			matchCount := 0
			for _, w := range ideaWords {
				if len(w) > 4 && strings.Contains(agentText, w) {
					matchCount++
				}
			}
			if matchCount >= 2 || strings.Contains(agentText, "inception") || strings.Contains(agentText, "clarif") {
				agentOnTask = true
				break
			}
		}
		if !agentOnTask {
			agentLines, _ := client.paneOutput("brainstorm")
			lastLine := ""
			if len(agentLines) > 0 {
				lastLine = agentLines[len(agentLines)-1]
			}
			return fail(result, "agent_error", fmt.Sprintf("brainstorm agent not processing inception idea after 60s (last: %s)", lastLine))
		}
	}

	// Step 3: Wait for clarify phase (bead watcher detects question beads)
	result.Phase = "capture_to_clarify"
	result.Check = "phase_advance"
	state, err = client.waitForPhase("clarify", 900*time.Second)
	if err != nil {
		// Check agent output for errors
		lines, _ := client.paneOutput("brainstorm")
		agentStatus := summarizeAgentOutput(lines)
		return fail(result, "timeout", fmt.Sprintf("capture→clarify: %v (agent: %s)", err, agentStatus))
	}

	// Verify questions
	result.Check = "questions_populated"
	questions, _ := state["questions"].([]interface{})
	const minExpectedQuestions = 5
	if len(questions) < minExpectedQuestions {
		return fail(result, "assertion", fmt.Sprintf("expected >=%d questions, got %d", minExpectedQuestions, len(questions)))
	}

	// Verify question structure
	result.Check = "question_structure"
	for i, q := range questions {
		qm, ok := q.(map[string]interface{})
		if !ok {
			return fail(result, "assertion", fmt.Sprintf("question %d is not a map", i))
		}
		if qm["id"] == nil || qm["id"].(string) == "" {
			return fail(result, "assertion", fmt.Sprintf("question %d missing id", i))
		}
		if qm["text"] == nil || qm["text"].(string) == "" {
			return fail(result, "assertion", fmt.Sprintf("question %d missing text", i))
		}
	}

	// CDP: verify clarify phase UI (questions visible)
	if cdpErr := verifyCDPPhase("clarify", pass); cdpErr != "" {
		t.Logf("Pass %d CDP warning (clarify): %s", pass, cdpErr)
	}

	// Step 4: Submit answers (use defaults) — always submit, even if phase
	// already advanced past clarify. The API accepts late answers and sets
	// PhaseChangedAt which the watcher needs to detect post-answer fact beads.
	result.Phase = "clarify"
	result.Check = "submit_answers"
	{
		answers := make(map[string]string)
		for _, q := range questions {
			qm := q.(map[string]interface{})
			id := qm["id"].(string)
			def, _ := qm["default"].(string)
			if def == "" {
				def = "default answer for " + id
			}
			answers[id] = def
		}
		data, code, err = client.post("/api/inception/answer", map[string]interface{}{"answers": answers})
		if err != nil {
			return fail(result, "api_error", fmt.Sprintf("answer failed: %v", err))
		}
		if code != 200 {
			return fail(result, "api_error", fmt.Sprintf("answer returned %d: %v", code, data))
		}
	}

	// Step 5: Wait for scaffold phase (bead watcher detects fact beads)
	result.Phase = "structure_to_scaffold"
	result.Check = "phase_advance"
	state, err = client.waitForPhase("scaffold", 900*time.Second)
	if err != nil {
		lines, _ := client.paneOutput("brainstorm")
		agentStatus := summarizeAgentOutput(lines)
		return fail(result, "timeout", fmt.Sprintf("structure→scaffold: %v (agent: %s)", err, agentStatus))
	}

	// CDP: verify scaffold phase UI
	if cdpErr := verifyCDPPhase("scaffold", pass); cdpErr != "" {
		t.Logf("Pass %d CDP warning (scaffold): %s", pass, cdpErr)
	}

	// Step 6: Verify scaffold
	result.Phase = "scaffold"
	result.Check = "scaffold_files"
	scaffoldData, code, err := client.get("/api/inception/scaffold")
	if err != nil || code != 200 {
		return fail(result, "api_error", fmt.Sprintf("scaffold returned %d: %v", code, err))
	}

	scaffold, _ := scaffoldData["scaffold"].(map[string]interface{})
	if scaffold == nil {
		return fail(result, "missing_content", "scaffold is nil")
	}
	files, _ := scaffold["files"].([]interface{})
	if len(files) == 0 {
		return fail(result, "missing_content", "scaffold has no files")
	}

	// Check each file has content
	result.Check = "scaffold_content"
	for _, f := range files {
		fm := f.(map[string]interface{})
		path, _ := fm["path"].(string)
		content, _ := fm["content"].(string)
		if content == "" {
			return fail(result, "missing_content", fmt.Sprintf("scaffold file %s has empty content", path))
		}
	}

	// Check required files exist
	result.Check = "required_files"
	fileContents := make(map[string]string)
	for _, f := range files {
		fm := f.(map[string]interface{})
		path, _ := fm["path"].(string)
		content, _ := fm["content"].(string)
		fileContents[path] = content
	}
	requiredFiles := []string{"README.md", "AGENTS.md", "CONTRIBUTING.md", ".github/workflows/ci.yml"}
	for _, req := range requiredFiles {
		if _, ok := fileContents[req]; !ok {
			return fail(result, "missing_content", fmt.Sprintf("missing required file: %s", req))
		}
	}

	// Verify content is project-specific, not boilerplate
	result.Check = "content_quality"
	readme := fileContents["README.md"]
	if len(readme) < 200 {
		return fail(result, "missing_content", fmt.Sprintf("README.md too short (%d chars) — boilerplate, not project-specific", len(readme)))
	}
	if strings.HasPrefix(strings.TrimSpace(readme), "# Project\n") {
		return fail(result, "missing_content", fmt.Sprintf("README.md starts with generic '# Project' — should have project name from vision"))
	}
	agentsMD := fileContents["AGENTS.md"]
	if len(agentsMD) < 200 {
		return fail(result, "missing_content", fmt.Sprintf("AGENTS.md too short (%d chars) — should have architecture, requirements, constraints", len(agentsMD)))
	}
	ciYml := fileContents[".github/workflows/ci.yml"]
	if !strings.Contains(ciYml, "steps:") {
		return fail(result, "missing_content", "ci.yml missing 'steps:' — likely empty or broken")
	}

	// Step 7: Approve
	result.Phase = "approve"
	result.Check = "approve_ok"
	data, code, err = client.post("/api/inception/approve", nil)
	if err != nil || code != 200 {
		return fail(result, "api_error", fmt.Sprintf("approve returned %d: %v", code, err))
	}

	// Verify complete
	result.Check = "phase_complete"
	state, err = client.inceptionState()
	if err != nil {
		return fail(result, "api_error", fmt.Sprintf("final state check: %v", err))
	}
	if state == nil || state["phase"] != "complete" {
		return fail(result, "assertion", fmt.Sprintf("expected phase=complete, got %v", state))
	}

	// CDP: verify complete phase UI
	if cdpErr := verifyCDPPhase("complete", pass); cdpErr != "" {
		t.Logf("Pass %d CDP warning (complete): %s", pass, cdpErr)
	}

	// Step 8: Check agent output for errors
	result.Phase = "agent_health"
	result.Check = "no_errors"
	lines, _ := client.paneOutput("brainstorm")
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "panic:") || strings.Contains(lower, "fatal error:") {
			return fail(result, "agent_error", fmt.Sprintf("agent output contains error: %s", line))
		}
	}

	// All checks passed
	result.Status = "pass"
	result.Phase = "complete"
	result.Check = "all_checks"
	return result
}

func fail(result PassResult, category, errMsg string) PassResult {
	result.Status = "fail"
	result.Category = category
	result.Error = errMsg
	return result
}

func summarizeAgentOutput(lines []string) string {
	if len(lines) == 0 {
		return "no output"
	}
	last := lines[len(lines)-1]
	if len(last) > 100 {
		last = last[:100]
	}
	return fmt.Sprintf("%d lines, last: %s", len(lines), last)
}
