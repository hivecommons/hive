package client

import (
	"context"
	"fmt"
	"net/url"
)

const (
	RunWaitingOnAgent  = "agent"
	RunWaitingOnRemote = "remote"
	RunWaitingOnHuman  = "human"
	RunWaitingOnCI     = "ci"
	RunWaitingOnNone   = "none"
)

type RunStage struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Gen     uint64 `json:"gen"`
	Receipt string `json:"receipt,omitempty"`
}

type Run struct {
	Key            string     `json:"key"`
	Title          string     `json:"title"`
	Repo           string     `json:"repo"`
	Stage          string     `json:"stage"`
	Gen            uint64     `json:"gen"`
	StageStartedAt string     `json:"stage_started_at,omitempty"`
	WaitingOn      string     `json:"waiting_on"`
	WaitingSince   string     `json:"waiting_since,omitempty"`
	Assignee       string     `json:"assignee,omitempty"`
	LastReceipt    string     `json:"last_receipt,omitempty"`
	PlanEpicID     string     `json:"plan_epic_id,omitempty"`
	Stages         []RunStage `json:"stages"`
}

type RoleInfo struct {
	Role        string `json:"role"`
	User        string `json:"user,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type PlanActionResult struct {
	OK     bool   `json:"ok"`
	Status string `json:"status"`
}

func (r RoleInfo) Owner() bool { return r.Role == "owner" || r.Role == "" }

func (c *Client) Runs(ctx context.Context) ([]Run, error) {
	var runs []Run
	if err := c.getJSON(ctx, "/api/runs", &runs); err != nil {
		return nil, err
	}
	return runs, nil
}

func (c *Client) Run(ctx context.Context, key string) (Run, error) {
	if key == "" {
		return Run{}, fmt.Errorf("GET /api/runs/{key}: run key is required")
	}
	var run Run
	if err := c.getJSON(ctx, "/api/runs/"+url.PathEscape(key), &run); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (c *Client) Role(ctx context.Context) (RoleInfo, error) {
	var role RoleInfo
	if err := c.getJSON(ctx, "/api/role", &role); err != nil {
		return RoleInfo{}, err
	}
	return role, nil
}

func (c *Client) ApproveRun(ctx context.Context, epicID string) (PlanActionResult, error) {
	return c.planAction(ctx, epicID, "approve")
}

func (c *Client) RejectRun(ctx context.Context, epicID string) (PlanActionResult, error) {
	return c.planAction(ctx, epicID, "reject")
}

func (c *Client) planAction(ctx context.Context, epicID, action string) (PlanActionResult, error) {
	if epicID == "" {
		return PlanActionResult{}, fmt.Errorf("POST /api/plan/{epicID}/%s: plan epic id is required", action)
	}
	var result PlanActionResult
	path := "/api/plan/" + url.PathEscape(epicID) + "/" + action
	if err := c.postJSON(ctx, path, nil, &result); err != nil {
		return PlanActionResult{}, err
	}
	return result, nil
}
