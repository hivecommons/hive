package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

// seamFaultScanner is a CanaryScanner whose Scan raises the row's fault
// first. A nil fault is a healthy scanner that finds nothing.
type seamFaultScanner struct{ f *seamFault }

func (s seamFaultScanner) Scan(string, string, string) (crux.CanaryLeak, bool) {
	s.f.raise()
	return crux.CanaryLeak{}, false
}

// seamFaultBody is a request body whose Read fails.
type seamFaultBody struct{}

func (seamFaultBody) Read([]byte) (int, error) { return 0, errors.New("injected body read fault") }

var errSeamFault = errors.New("injected seam error")

// seamFault is one injected failure: an error, a panic, or a call that does
// not return before the seam's deadline. A nil *seamFault is healthy.
type seamFault struct {
	kind    string // "error", "panic" or "timeout"
	release <-chan struct{}
}

// raise performs the fault. It returns errSeamFault for the error kind, nil
// when healthy, and never returns normally for panic (or until release closes
// for timeout).
func (f *seamFault) raise() error {
	if f == nil {
		return nil
	}
	switch f.kind {
	case "error":
		return errSeamFault
	case "panic":
		panic("injected policy seam fault")
	case "timeout":
		<-f.release
	}
	return nil
}

// TestPolicySeamFaultInjectionDenies covers the decision-path seams that are
// not Set*Func predicates (#19224; TestPolicyCallbackFailureDenies covers
// those). Every seam is driven with each fault it can raise (an error, a
// panic, a call past its deadline) and the gate must refuse. Each seam also
// runs healthy first and must allow the same request, so a refusal is the
// fault's doing and not a blanket deny. Before the guard, the panic rows
// crashed the test binary and the timeout rows blocked until the test
// timeout.
func TestPolicySeamFaultInjectionDenies(t *testing.T) {
	prevPolicy, prevScan, prevLookup := policyCallbackTimeout, canaryScanTimeout, referenceLookupTimeout
	policyCallbackTimeout, canaryScanTimeout, referenceLookupTimeout = 50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		policyCallbackTimeout, canaryScanTimeout, referenceLookupTimeout = prevPolicy, prevScan, prevLookup
	})
	errKind := &seamFault{kind: "error"}
	panicKind := &seamFault{kind: "panic"}
	timeoutKind := &seamFault{kind: "timeout", release: release}

	replyPath := "/repos/cncf/techdocs/pulls/comments/" + rtReplyID + "/replies"
	deleteOwnPath := "/repos/cncf/techdocs/issues/comments/" + rtCommentA
	refPol := ReferenceRepoPolicy{Repo: "hivecommons/docs", Issues: true, Login: "hivecommons-hive[bot]", Token: func() (string, bool) { return "ref-tok", true }}

	seams := []struct {
		name   string
		faults []*seamFault
		// check evaluates the gate with the seam raising f (nil = healthy)
		// and reports the refusal reason and whether it refused.
		check func(t *testing.T, f *seamFault) (string, bool)
		// failReason, when set, is the exact reason a faulted call must give.
		failReason string
	}{
		{
			name:   "review-thread OwnPR",
			faults: []*seamFault{panicKind, timeoutKind},
			check: func(t *testing.T, f *seamFault) (string, bool) {
				p := newRTProxy(t, newRTFixture(time.Now()), nil)
				g := p.reviewThreadGuard()
				g.OwnPR = func(repo string, n int) (string, bool) {
					_ = f.raise()
					return rtOwnPR(repo, n)
				}
				p.SetReviewThreadGuard(g)
				return p.reviewThreadRefusal(rtReq("POST", replyPath, `{"body":"x"}`), rtOwner)
			},
			failReason: policyCallbackFailedReason(reviewOwnershipGate),
		},
		{
			name:   "review-thread HiveLogins",
			faults: []*seamFault{panicKind, timeoutKind},
			check: func(t *testing.T, f *seamFault) (string, bool) {
				p := newRTProxy(t, newRTFixture(time.Now()), nil)
				g := p.reviewThreadGuard()
				g.HiveLogins = func() []string {
					_ = f.raise()
					return []string{rtHiveBot}
				}
				p.SetReviewThreadGuard(g)
				return p.reviewThreadRefusal(rtReq("DELETE", deleteOwnPath, ""), rtOwner)
			},
			failReason: policyCallbackFailedReason(reviewOwnershipGate),
		},
		{
			name:   "review-thread comment lookup",
			faults: []*seamFault{errKind, panicKind, timeoutKind},
			check: func(t *testing.T, f *seamFault) (string, bool) {
				fx := newRTFixture(time.Now())
				p := newRTProxy(t, fx, nil)
				p.referenceLookup = func(url, token string) ([]byte, error) {
					if err := f.raise(); err != nil {
						return nil, err
					}
					return fx.lookup(url, token)
				}
				return p.reviewThreadRefusal(rtReq("POST", replyPath, `{"body":"x"}`), rtOwner)
			},
		},
		{
			name:   "reference repo own-item lookup",
			faults: []*seamFault{errKind, panicKind, timeoutKind},
			check: func(t *testing.T, f *seamFault) (string, bool) {
				p := newReferenceTestProxy(t, refPol, func(string, string) ([]byte, error) {
					if err := f.raise(); err != nil {
						return nil, err
					}
					return []byte(`{"user":{"login":"hivecommons-hive[bot]"}}`), nil
				})
				return p.referenceRepoRefusal(refReq("POST", "/repos/hivecommons/docs/issues/12/comments", "", nil))
			},
		},
		{
			// Advisory canary mode (failClosed=false) still refuses a scan
			// that could not run: advisory governs what a found leak does,
			// and an unscanned body has not been found clean.
			name:   "canary scan",
			faults: []*seamFault{errKind, panicKind, timeoutKind},
			check: func(t *testing.T, f *seamFault) (string, bool) {
				p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
				req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues/1/comments", strings.NewReader(`{"body":"hello"}`))
				scanFault := f
				if f != nil && f.kind == "error" {
					// Scan has no error return; this seam's error branch is
					// the body it would scan being unreadable.
					req.Body = io.NopCloser(seamFaultBody{})
					scanFault = nil
				}
				p.SetCanaryScanner(true, false, seamFaultScanner{f: scanFault}, nil)
				reason, deny, _ := p.inspectCanaryEgress("scanner", req)
				return reason, deny
			},
		},
	}

	for _, s := range seams {
		t.Run(s.name+"/healthy", func(t *testing.T) {
			if reason, denied := s.check(t, nil); denied {
				t.Fatalf("healthy %s refused: %q", s.name, reason)
			}
		})
		for _, f := range s.faults {
			t.Run(s.name+"/"+f.kind, func(t *testing.T) {
				reason, denied := s.check(t, f)
				if !denied {
					t.Fatalf("%s %s: gate allowed the request; want deny", s.name, f.kind)
				}
				if s.failReason != "" && reason != s.failReason {
					t.Fatalf("%s %s: reason = %q; want %q", s.name, f.kind, reason, s.failReason)
				}
			})
		}
	}
}
