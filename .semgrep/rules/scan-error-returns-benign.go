// Fixture for hive-scan-error-returns-benign. Not compiled: semgrep --test
// parses it and checks each ruleid/ok annotation against the line below it.
package ioscan

import (
	"context"
	"errors"
)

func (c *LLMClassifier) scoreSwallowsError(ctx context.Context, text string) (InjectionScore, error) {
	reply, err := c.client.Complete(ctx, text)
	if err != nil {
		// ruleid: hive-scan-error-returns-benign
		return InjectionScore{Category: classifierCategoryBenign}, nil
	}
	return ParseInjectionScore(reply)
}

func (c *LLMClassifier) scoreSwallowsErrorLiteral(ctx context.Context, text string) (InjectionScore, error) {
	reply, err := c.client.Complete(ctx, text)
	if err != nil {
		// ruleid: hive-scan-error-returns-benign
		return InjectionScore{Score: 0, Category: "benign", Rationale: "classifier down"}, nil
	}
	return ParseInjectionScore(reply)
}

func (c *LLMClassifier) scoreZeroOnError(ctx context.Context, text string) (InjectionScore, error) {
	reply, err := c.client.Complete(ctx, text)
	if err != nil {
		// ruleid: hive-scan-error-returns-benign
		return InjectionScore{}, nil
	}
	return ParseInjectionScore(reply)
}

func scanZeroVerdictOnError(text string) (string, Verdict) {
	findings, err := collect(text)
	if err != nil {
		// ruleid: hive-scan-error-returns-benign
		return text, Verdict{}
	}
	return text, Verdict{Findings: findings}
}

func scanNotBlockedOnError(text string) Verdict {
	findings, err := collect(text)
	if err != nil {
		// ruleid: hive-scan-error-returns-benign
		return Verdict{Blocked: false}
	}
	return Verdict{Findings: findings, Blocked: len(findings) > 0}
}

func gateSafeOnError(text string) (ScanResult, error) {
	ok, err := probe(text)
	if err != nil {
		// ruleid: hive-scan-error-returns-benign
		return ScanResult{Safe: true, Reason: "probe failed"}, nil
	}
	return ScanResult{Safe: ok}, nil
}

// The fixed shape: the error goes back to the caller, which applies the fail
// mode.
func (c *LLMClassifier) scorePropagates(ctx context.Context, text string) (InjectionScore, error) {
	reply, err := c.client.Complete(ctx, text)
	if err != nil {
		// ok: hive-scan-error-returns-benign
		return InjectionScore{}, err
	}
	return ParseInjectionScore(reply)
}

// A value returned beside the error is fine: the caller must check err.
func scanWithError(text string) (Verdict, error) {
	findings, err := collect(text)
	if err != nil {
		// ok: hive-scan-error-returns-benign
		return Verdict{Blocked: false}, err
	}
	return Verdict{Findings: findings}, nil
}

// Failing closed is what an error branch should do.
func scanWithholdsOnError(text string) (string, Verdict) {
	_, err := collect(text)
	if err != nil {
		// ok: hive-scan-error-returns-benign
		return UnavailableRedactionMarker("error"), UnavailableVerdict(err.Error())
	}
	return text, Verdict{}
}

// A blocked verdict on error is fail-closed, not benign.
func scanBlocksOnError(text string) Verdict {
	_, err := collect(text)
	if err != nil {
		// ok: hive-scan-error-returns-benign
		return Verdict{Blocked: true}
	}
	return Verdict{}
}

// Outside an error branch a zero verdict is just "nothing found".
func emptyText(text string) (InjectionScore, error) {
	if text == "" {
		// ok: hive-scan-error-returns-benign
		return InjectionScore{Category: classifierCategoryBenign}, nil
	}
	return InjectionScore{}, errors.New("not implemented")
}
