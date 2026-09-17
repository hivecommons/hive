package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The advisory digest posting policy moved to pkg/advisory (#7238 stage 2) and
// the tests that covered it moved with it. Two things were left behind here,
// and neither is covered by those relocated tests -- they are properties of the
// wrappers, not of the policy.
//
// This file covers exactly that seam.

// TestAdvisoryPostGateIsSharedAcrossCalls pins the property the extraction most
// easily destroys.
//
// advisoryPostDue used to read a package-level struct, so its state obviously
// outlived a call. It now delegates to an owned *advisory.PostGate, and the
// throttle only works if every call reaches the SAME instance. Building the
// gate inside the wrapper -- advisory.NewPostGate().Due(...) -- compiles, reads
// perfectly naturally, and is completely wrong: a fresh gate has no recorded
// success, so it reports due every single time and governor.advisory
// .update_interval_s (#4820) silently stops throttling.
//
// Nothing would fail. The digest would just post on every eval cycle again,
// which is precisely the GitHub round-trip cost #4820 exists to bound, and the
// only symptom would be API traffic nobody is watching. So the sharing is
// asserted rather than assumed.
func TestAdvisoryPostGateIsSharedAcrossCalls(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// An hour is far longer than the test takes, so a gate that remembers the
	// success below cannot legitimately reopen during this test.
	cfg := config.AdvisoryConfig{UpdateIntervalS: 3600}
	const repo = "org/gate-sharing"
	now := time.Now()

	if !advisoryPostDue(cfg, repo, now, logger) {
		t.Fatal("first attempt for a repo must be due — nothing has succeeded yet")
	}

	recordAdvisoryPostSuccess(repo, now)

	if advisoryPostDue(cfg, repo, now.Add(time.Minute), logger) {
		t.Fatal("gate reopened one minute into a one-hour interval: advisoryPostDue " +
			"and recordAdvisoryPostSuccess are not reaching the same PostGate instance")
	}

	// A different repo must still be due, confirming the gate is shared but
	// per-repo — a single global timestamp would wrongly throttle this too.
	if !advisoryPostDue(cfg, "org/other", now.Add(time.Minute), logger) {
		t.Error("a repo that never posted must be due; the gate is keyed per repo")
	}

	// And the interval genuinely elapsing must reopen it, so the guard above is
	// pinning "remembers the success", not "always closed".
	if !advisoryPostDue(cfg, repo, now.Add(2*time.Hour), logger) {
		t.Error("gate must reopen once the configured interval has elapsed")
	}
}

// TestAdvisoryDigestWrappersTranslateNilClient covers the one piece of logic the
// wrappers actually own.
//
// pkg/advisory cannot import pkg/github (pkg/github imports pkg/advisory), so
// the digest predicates take a bool and the wrappers perform the `ghClient !=
// nil` translation. The relocated tests exercise the bool form directly and so
// can never catch that translation being inverted or hard-coded — which would
// turn "no client, so the write cannot be attempted" into "client present",
// resurrecting the empty-digest-from-nothing case the predicates exist to
// prevent.
func TestAdvisoryDigestWrappersTranslateNilClient(t *testing.T) {
	client := &github.Client{}
	empty := &advisory.Digest{}

	if !shouldBuildAdvisoryDigest(nil, client, true) {
		t.Error("a non-nil client with an existing pinned issue must build")
	}
	if shouldBuildAdvisoryDigest(nil, nil, true) {
		t.Error("a nil client must NOT be translated as client-present")
	}
	if !shouldPostAdvisoryDigest(empty, client, true) {
		t.Error("a non-nil client with a pinned issue must post the empty digest")
	}
	if shouldPostAdvisoryDigest(empty, nil, true) {
		t.Error("a nil client must NOT be translated as client-present")
	}
}
