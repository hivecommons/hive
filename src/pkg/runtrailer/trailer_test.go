package runtrailer

import "testing"

func TestParseSharedGrammarAcceptsCurrentAgentauditInputs(t *testing.T) {
	message := "subject\n\n Hive-Run : o/r#1 \nHive-Plan: first\nHive-Plan: last\r\nHive-Spec: spec.md#C-1\r\n"
	trailers := Parse(message, LastWins)
	if trailers[KeyRun] != "o/r#1" || trailers[KeyPlan] != "last" || trailers[KeySpec] != "spec.md#C-1" {
		t.Fatalf("Parse LastWins = %+v", trailers)
	}
}

func TestParseSharedGrammarAcceptsCurrentGithubInputs(t *testing.T) {
	message := "Some prose that mentions Hive-Run: in passing is not a trailer line.\n\nHive-Run:\nHive-Run: o/r#2\r\nHive-Plan: first\nHive-Plan: second\n"
	trailers := Parse(message, FirstWins)
	if trailers[KeyRun] != "o/r#2" || trailers[KeyPlan] != "first" {
		t.Fatalf("Parse FirstWins = %+v", trailers)
	}
}

func TestParseSharedGrammarRejectsLowercaseKeys(t *testing.T) {
	trailers := Parse("hive-run: o/r#1\nHIVE-PLAN: p1\n", FirstWins)
	if len(trailers) != 0 {
		t.Fatalf("Parse accepted non-canonical keys: %+v", trailers)
	}
}
