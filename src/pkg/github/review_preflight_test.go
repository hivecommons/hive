package github

import (
	"context"
	"testing"
)

func TestReviewPreflightReadsFailClosedWithoutAClient(t *testing.T) {
	ctx := context.Background()
	var nilClient *Client
	if _, ok := nilClient.ReviewPreflightFile(ctx, "o/r", "abc", "README.md"); ok {
		t.Fatal("a nil client cannot read a file")
	}
	if _, ok := (&Client{}).ReviewPreflightFile(ctx, "o/r", "", "README.md"); ok {
		t.Fatal("no ref means no read")
	}
	if _, ok := nilClient.ReviewPreflightTree(ctx, "o/r", "abc"); ok {
		t.Fatal("a nil client cannot list a tree")
	}
}
