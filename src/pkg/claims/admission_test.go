package claims

import (
	"errors"
	"testing"
)

func TestAdmissionRefusesAutomatedClaims(t *testing.T) {
	for _, reason := range []string{"hold", "needs-human"} {
		for _, kind := range []Kind{KindAgent, KindContributor, KindExternal} {
			t.Run(reason+"/"+string(kind), func(t *testing.T) {
				writes := 0
				l, _ := newTestLedger(t, Hooks{OnClaimed: func(Claim, Outcome) { writes++ }})
				l.SetAdmissionCheck(func(Request) (string, error) { return reason, nil })
				res, err := l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "worker", Kind: kind, Force: true})
				if err != nil || res.Outcome != OutcomeRefused || res.Reason != reason {
					t.Fatalf("claim = %+v, %v", res, err)
				}
				if len(l.List()) != 0 || writes != 0 {
					t.Fatal("refused claim mutated ledger or fired write hook")
				}
			})
		}
	}
}

func TestAdmissionRefusesRenewalAndTakeover(t *testing.T) {
	l, _ := newTestLedger(t, Hooks{})
	req := Request{Repo: "o/r", Issue: 1, Holder: "worker", Kind: KindContributor}
	first, _ := l.Claim(req)
	l.SetAdmissionCheck(func(Request) (string, error) { return "needs-human", nil })
	for _, next := range []Request{req, {Repo: "o/r", Issue: 1, Holder: "scanner", Kind: KindAgent, Force: true}} {
		res, err := l.Claim(next)
		if err != nil || res.Outcome != OutcomeRefused {
			t.Fatalf("claim = %+v, %v", res, err)
		}
		if got, _ := l.Lookup("o/r", 1); got != first.Claim {
			t.Fatal("refusal changed existing claim")
		}
	}
}

func TestReleaseBlockedClaims(t *testing.T) {
	released := 0
	l, _ := newTestLedger(t, Hooks{OnReleased: func(c Claim, reason string) {
		if c.Kind == KindHuman || reason != "needs-human" {
			t.Errorf("unexpected release: %+v, %q", c, reason)
		}
		released++
	}})
	for i, kind := range []Kind{KindAgent, KindContributor, KindExternal, KindHuman} {
		_, _ = l.Claim(Request{Repo: "o/r", Issue: i + 1, Holder: "worker", Kind: kind})
	}
	l.SetAdmissionCheck(func(req Request) (string, error) {
		if req.Issue == 3 {
			return "", errors.New("forge unavailable")
		}
		return "needs-human", nil
	})
	if n := l.ReleaseBlocked(); n != 2 || released != 2 {
		t.Fatalf("released %d, hooks %d; want 2", n, released)
	}
	if _, ok := l.Lookup("o/r", 3); !ok {
		t.Fatal("lookup error released a claim")
	}
	if _, ok := l.Lookup("o/r", 4); !ok {
		t.Fatal("needs-human released a human claim")
	}
	res, err := l.Claim(Request{Repo: "o/r", Issue: 5, Holder: "person", Kind: KindHuman})
	if err != nil || res.Outcome != OutcomeClaimed {
		t.Fatalf("human claim refused: %+v, %v", res, err)
	}
	res, err = l.Claim(Request{Repo: "o/r", Issue: 3, Holder: "other", Kind: KindAgent})
	if err == nil || res.Outcome.Changed() {
		t.Fatalf("lookup failure admitted claim: %+v, %v", res, err)
	}
}

func TestReleaseBlockedPreservesConcurrentTakeover(t *testing.T) {
	l, _ := newTestLedger(t, Hooks{})
	_, _ = l.Claim(Request{Repo: "o/r", Issue: 1, Holder: "scanner", Kind: KindAgent})
	l.SetAdmissionCheck(func(req Request) (string, error) {
		// Human takeover while the forge check is in progress.
		_, _ = l.Claim(Request{Repo: req.Repo, Issue: req.Issue, Holder: "person", Kind: KindHuman})
		return "needs-human", nil
	})
	if n := l.ReleaseBlocked(); n != 0 {
		t.Fatalf("released %d concurrent claims", n)
	}
	if c, ok := l.Lookup("o/r", 1); !ok || c.Kind != KindHuman {
		t.Fatalf("human takeover lost: %+v, %v", c, ok)
	}
}
