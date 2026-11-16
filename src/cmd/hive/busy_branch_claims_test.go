package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
)

// hivecommons/hive#18488: the busy-branch guard reads real claims from the
// worker-claim ledger — never kick listings — and reports which hive agent
// holds one so the kicked agent's own claim is not mistaken for another
// worker's.

func TestClaimsPRLeaseLookup(t *testing.T) {
	var none *claims.Ledger
	if claimsPRLeaseLookup(none, "hivecommons") != nil {
		t.Fatal("no ledger must install no lookup")
	}
	l, err := claims.New(filepath.Join(t.TempDir(), "claims.json"), claims.DefaultPolicy(), claims.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []claims.Request{
		{Repo: "hivecommons/hive", Issue: 18488, Holder: "quality", HolderID: "quality", Kind: claims.KindAgent},
		{Repo: "hivecommons/hive", Issue: 18441, Holder: "clubanderson", Kind: claims.KindHuman},
	} {
		if res, err := l.Claim(req); err != nil || res.Outcome != claims.OutcomeClaimed {
			t.Fatalf("claim %+v: outcome=%s err=%v", req, res.Outcome, err)
		}
	}
	lookup := claimsPRLeaseLookup(l, "hivecommons")
	// A bare governor repo name is qualified with the org, as the in-flight
	// lookup does.
	if holder, agent, ok := lookup("hive", 18488); !ok || agent != "quality" || !strings.Contains(holder, "quality") {
		t.Fatalf("agent claim: holder=%q agent=%q ok=%v", holder, agent, ok)
	}
	if holder, agent, ok := lookup("hivecommons/hive", 18441); !ok || agent != "" || !strings.Contains(holder, "clubanderson") {
		t.Fatalf("human claim: holder=%q agent=%q ok=%v (a person is never reported as a hive agent)", holder, agent, ok)
	}
	if _, _, ok := lookup("hivecommons/hive", 1); ok {
		t.Fatal("an unclaimed number must not read as busy")
	}
}
