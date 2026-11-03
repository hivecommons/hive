package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

func lanePath(lane string) string { return PathLanes + "/" + url.PathEscape(lane) }
func laneGroupPath(lane, group string) string {
	return lanePath(lane) + "/groups/" + url.PathEscape(group)
}

func (c *Client) LaneRegistry(ctx context.Context) ([]Lane, error) {
	var out []Lane
	err := c.Do(ctx, http.MethodGet, PathLanes, nil, &out)
	return out, err
}
func (c *Client) PublishLane(ctx context.Context, lane string, in LanePublish) (LanePublished, error) {
	var out LanePublished
	err := c.Do(ctx, http.MethodPost, lanePath(lane)+"/publish", in, &out)
	return out, err
}
func (c *Client) ReplayLane(ctx context.Context, lane string, cursor uint64, limit int) (LaneReplay, error) {
	var out LaneReplay
	err := c.Do(ctx, http.MethodGet, lanePath(lane)+fmt.Sprintf("/events?cursor=%d&limit=%d", cursor, limit), nil, &out)
	return out, err
}

// ClaimLane returns nil, nil when no work is currently claimable (HTTP 204).
func (c *Client) ClaimLane(ctx context.Context, lane, group string) (*LaneClaim, error) {
	var out *LaneClaim
	err := c.Do(ctx, http.MethodPost, laneGroupPath(lane, group)+"/claim", nil, &out)
	return out, err
}
func (c *Client) AckLane(ctx context.Context, lane, group string, in LaneReceipt) error {
	return c.Do(ctx, http.MethodPost, laneGroupPath(lane, group)+"/ack", in, nil)
}
func (c *Client) NackLane(ctx context.Context, lane, group string, in LaneReceipt) error {
	return c.Do(ctx, http.MethodPost, laneGroupPath(lane, group)+"/nack", in, nil)
}
func (c *Client) RenewLane(ctx context.Context, lane, group string, in LaneReceipt) (LaneRenewed, error) {
	var out LaneRenewed
	err := c.Do(ctx, http.MethodPost, laneGroupPath(lane, group)+"/renew", in, &out)
	return out, err
}

func (c *Client) InspectLaneGroup(ctx context.Context, lane, group string) (LaneGroupState, error) {
	var out LaneGroupState
	err := c.Do(ctx, http.MethodGet, laneGroupPath(lane, group), nil, &out)
	return out, err
}

func (c *Client) RecoverLaneGroup(ctx context.Context, lane, group string, in LaneRecover) (LaneRecovered, error) {
	var out LaneRecovered
	err := c.Do(ctx, http.MethodPost, laneGroupPath(lane, group)+"/recover", in, &out)
	return out, err
}
