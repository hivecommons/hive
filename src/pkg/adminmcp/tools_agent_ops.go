package adminmcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	WriteOpAgentPause  = "agent.pause"
	WriteOpAgentResume = "agent.resume"
)

func DefaultWriteOps() []WriteOp {
	ops := append([]WriteOp{agentPauseOp{}, agentResumeOp{}}, repoOpsWriteOps()...)
	return append(ops, fleetWriteOps()...)
}

type agentPauseOp struct{}
type agentResumeOp struct{}

func (agentPauseOp) Name() string                { return WriteOpAgentPause }
func (agentPauseOp) Description() string         { return "Pause one agent through POST /api/pause/{agent}." }
func (agentPauseOp) InputSchema() map[string]any { return agentOpSchema() }
func (agentPauseOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent := stringArg(args, "agent")
	if agent == "" {
		return WritePreview{}, fmt.Errorf("agent is required")
	}
	path := "/api/pause/" + url.PathEscape(agent)
	return WritePreview{Operation: WriteOpAgentPause, Summary: "Pause agent " + agent, Target: agent, Request: WriteRequest{Method: http.MethodPost, Path: path}, Effects: []string{"The named agent stops accepting new work until resumed."}, WideningDisclosure: "No widening: this operation targets exactly one named agent.", ConfirmationMessage: "Confirm pausing agent " + agent + ".", Details: map[string]any{"agent": agent}}, nil
}

func (agentResumeOp) Name() string { return WriteOpAgentResume }
func (agentResumeOp) Description() string {
	return "Resume one agent through POST /api/resume/{agent}."
}
func (agentResumeOp) InputSchema() map[string]any { return agentOpSchema() }
func (agentResumeOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent := stringArg(args, "agent")
	if agent == "" {
		return WritePreview{}, fmt.Errorf("agent is required")
	}
	path := "/api/resume/" + url.PathEscape(agent)
	return WritePreview{Operation: WriteOpAgentResume, Summary: "Resume agent " + agent, Target: agent, Request: WriteRequest{Method: http.MethodPost, Path: path}, Effects: []string{"The named agent may accept work again."}, WideningDisclosure: "No widening: this operation targets exactly one named agent.", ConfirmationMessage: "Confirm resuming agent " + agent + ".", Details: map[string]any{"agent": agent}}, nil
}

func agentOpSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"agent": map[string]any{"type": "string", "minLength": 1}}, "required": []string{"agent"}, "additionalProperties": false}
}

func cleanOperationName(name string) string { return strings.TrimSpace(name) }
