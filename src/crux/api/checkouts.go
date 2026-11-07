package api

import (
	"context"
	"net/url"
	"time"
)

// Pinned checkouts (hivecommons/crux#114 S1): "a working copy of this repo at
// this commit, owned by this caller, capped, deletable". Routes are a
// work-services family: lane project credentials are accepted here, with
// the project as the owner, and the control token acts as the operator.
const (
	// PathCheckouts is POST (create), GET (list, ?owner=) /v1/checkouts and
	// GET/DELETE /v1/checkouts/{id}.
	PathCheckouts = "/v1/checkouts"
)

// Checkout error codes.
const (
	// CodeTooLarge is a checkout that exceeded max_bytes once measured; the
	// clone has been removed (413).
	CodeTooLarge = "too_large"
	// CodeUnsupported is a requested limit the backend could not enforce or
	// verify (501); the call was refused, never run with the limit dropped.
	CodeUnsupported = "unsupported"
	// CodeUpstream is a backend answer that contradicts the request, such as
	// a worktree at a different commit than the one pinned (502).
	CodeUpstream = "upstream_error"
)

// CheckoutRequest is the body of POST /v1/checkouts.
type CheckoutRequest struct {
	// Repo is the repository URL; it must match a configured allowlist prefix.
	Repo string `json:"repo"`
	// Commit is the full 40-hex SHA to pin; tags and branches are refused.
	Commit string `json:"commit"`
	// Owner identifies the owning run or driver. A project principal may
	// only name itself (or leave it empty); the operator names anyone and
	// defaults to its own name.
	Owner string `json:"owner,omitempty"`
	// Branches clones all refs when nil or true; false clones one branch.
	Branches *bool `json:"branches,omitempty"`
	// MaxBytes caps the clone plus worktree; zero means the server default.
	MaxBytes int64 `json:"max_bytes,omitempty"`
	// TTL is a Go duration ("72h") after which a sweeper may reclaim it.
	TTL string `json:"ttl,omitempty"`
}

// Checkout is one checkout record: the response of POST and GET.
type Checkout struct {
	ID        string     `json:"id"`
	Path      string     `json:"path"`
	Repo      string     `json:"repo"`
	Commit    string     `json:"commit"`
	Owner     string     `json:"owner"`
	Branches  bool       `json:"branches"`
	MaxBytes  int64      `json:"max_bytes"`
	Bytes     int64      `json:"bytes"`
	TTL       string     `json:"ttl,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// CheckoutList is the response of GET /v1/checkouts.
type CheckoutList struct {
	Checkouts []Checkout `json:"checkouts"`
}

// CreateCheckout is POST /v1/checkouts.
func (c *Client) CreateCheckout(ctx context.Context, in CheckoutRequest) (Checkout, error) {
	var out Checkout
	err := c.do(ctx, "POST", PathCheckouts, in, &out)
	return out, err
}

// ListCheckouts is GET /v1/checkouts[?owner=].
func (c *Client) ListCheckouts(ctx context.Context, owner string) ([]Checkout, error) {
	path := PathCheckouts
	if owner != "" {
		path += "?owner=" + url.QueryEscape(owner)
	}
	var out CheckoutList
	err := c.do(ctx, "GET", path, nil, &out)
	return out.Checkouts, err
}

// GetCheckout is GET /v1/checkouts/{id}.
func (c *Client) GetCheckout(ctx context.Context, id string) (Checkout, error) {
	var out Checkout
	err := c.do(ctx, "GET", PathCheckouts+"/"+url.PathEscape(id), nil, &out)
	return out, err
}

// DeleteCheckout is DELETE /v1/checkouts/{id}.
func (c *Client) DeleteCheckout(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", PathCheckouts+"/"+url.PathEscape(id), nil, nil)
}
