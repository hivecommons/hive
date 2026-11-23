package factcheck

import "testing"

func TestQualifies(t *testing.T) {
	p := Policy{AuthorAgents: []string{"quality"}, Labels: []string{"analysis"}}
	tests := []struct {
		name   string
		policy Policy
		pr     PR
		author string
		want   bool
	}{
		{"qualifying agent", p, PR{Open: true}, "quality", true},
		{"agent match is case-insensitive", p, PR{Open: true}, "Quality", true},
		{"replica of the qualifying agent", p, PR{Open: true}, "quality-2", true},
		{"other agent without label", p, PR{Open: true}, "scanner", false},
		{"other agent with qualifying label", p, PR{Open: true, Labels: []string{"docs", "Analysis"}}, "scanner", true},
		{"unknown author with label", p, PR{Open: true, Labels: []string{"analysis"}}, "", true},
		{"unknown author without label", p, PR{Open: true}, "", false},
		{"closed PR never qualifies", p, PR{Open: false}, "quality", false},
		{"no qualifiers configured", Policy{}, PR{Open: true, Labels: []string{"analysis"}}, "quality", false},
		{"similar agent name is not a replica", p, PR{Open: true}, "quality-gate", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := Qualifies(tt.policy, tt.pr, tt.author)
			if got != tt.want {
				t.Fatalf("Qualifies = %v (%s), want %v", got, reason, tt.want)
			}
			if reason == "" {
				t.Fatal("a reason is always given")
			}
		})
	}
}

func TestCheckIndependent(t *testing.T) {
	author := Agent{Name: "quality", Model: "claude-opus-4-6"}
	tests := []struct {
		name     string
		level    Independence
		author   Agent
		reviewer Agent
		pin      string
		want     bool
	}{
		{"author never reviews itself, agent level", IndependenceAgent, author, Agent{Name: "quality", Model: "gpt-6-sol"}, "", false},
		{"replica of the author is the author", IndependenceModel, author, Agent{Name: "quality-2", Model: "gpt-6-sol"}, "", false},
		{"different agent passes agent level on same model", IndependenceAgent, author, Agent{Name: "reviewer", Model: "claude-opus-4-6"}, "", true},
		{"same model refused at model level", IndependenceModel, author, Agent{Name: "reviewer", Model: "claude-opus-4-6"}, "", false},
		{"same model in another spelling refused", IndependenceModel, author, Agent{Name: "reviewer", Model: "anthropic/claude-opus-4-6"}, "", false},
		{"different model passes model level", IndependenceModel, author, Agent{Name: "reviewer", Model: "claude-fable-5"}, "", true},
		{"same family refused at family level", IndependenceFamily, author, Agent{Name: "reviewer", Model: "claude-fable-5"}, "", false},
		{"other family passes family level", IndependenceFamily, author, Agent{Name: "reviewer", Model: "gpt-6.1-sol"}, "", true},
		{"unknown reviewer model refused at model level", IndependenceModel, author, Agent{Name: "reviewer"}, "", false},
		{"unknown author model refused at model level", IndependenceModel, Agent{Name: "quality"}, Agent{Name: "reviewer", Model: "gpt-6.1-sol"}, "", false},
		{"unknown models fine at agent level", IndependenceAgent, Agent{Name: "quality"}, Agent{Name: "reviewer"}, "", true},
		{"pinned model rescues an unknown reviewer model", IndependenceModel, author, Agent{Name: "reviewer"}, "gpt-6.1-sol", true},
		{"pinned model equal to author's is refused", IndependenceModel, author, Agent{Name: "reviewer", Model: "gpt-6.1-sol"}, "claude-opus-4-6", false},
		{"pinned same-family model refused at family level", IndependenceFamily, author, Agent{Name: "reviewer", Model: "gpt-6.1-sol"}, "claude-fable-5", false},
		{"no agent author accepts any reviewer", IndependenceFamily, Agent{}, Agent{Name: "reviewer", Model: "claude-opus-4-6"}, "", true},
		// CLI aliases are their real family: Gemini on Gemini and Claude on
		// Claude are refused even when the author ran a bare alias.
		{"gemini flash alias author refuses a gemini reviewer", IndependenceFamily, Agent{Name: "quality", Model: "flash"}, Agent{Name: "reviewer", Model: "gemini-2.5-flash"}, "", false},
		{"gemini pro alias author refuses a gemini reviewer", IndependenceFamily, Agent{Name: "quality", Model: "pro"}, Agent{Name: "reviewer", Model: "gemini-2.5-pro"}, "", false},
		{"claude opus alias author refuses a claude reviewer", IndependenceFamily, Agent{Name: "quality", Model: "opus"}, Agent{Name: "reviewer", Model: "claude-opus-5-5"}, "", false},
		{"gemini alias author accepts a claude reviewer", IndependenceFamily, Agent{Name: "quality", Model: "flash"}, Agent{Name: "reviewer", Model: "claude-opus-5-5"}, "", true},
		{"auto author still refused at family level", IndependenceFamily, Agent{Name: "quality", Model: "auto"}, Agent{Name: "reviewer", Model: "claude-opus-5-5"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := CheckIndependent(tt.level, tt.author, tt.reviewer, tt.pin)
			if got != tt.want {
				t.Fatalf("CheckIndependent = %v (%q), want %v", got, why, tt.want)
			}
			if !got && why == "" {
				t.Fatal("a refusal names its reason")
			}
		})
	}
}

func TestPickReviewer(t *testing.T) {
	author := Agent{Name: "quality", Model: "claude-opus-4-6"}
	same := Agent{Name: "reviewer", Model: "claude-opus-4-6"}
	other := Agent{Name: "guide", Model: "gpt-6.1-sol"}
	self := Agent{Name: "quality", Model: "gpt-6.1-sol"}
	p := Policy{Independence: IndependenceModel}

	if got, _, ok := PickReviewer(p, author, []Agent{self, same, other}); !ok || got.Name != "guide" {
		t.Fatalf("want the first independent candidate (guide), got %+v ok=%v", got, ok)
	}
	if got, _, ok := PickReviewer(p, author, []Agent{other, same}); !ok || got.Name != "guide" {
		t.Fatalf("preference order kept, got %+v ok=%v", got, ok)
	}
	if _, why, ok := PickReviewer(p, author, []Agent{same}); ok || why == "" {
		t.Fatalf("a same-model lane must be refused with a reason, ok=%v why=%q", ok, why)
	}
	if _, why, ok := PickReviewer(p, author, nil); ok || why == "" {
		t.Fatalf("no candidates must be refused with a reason, ok=%v why=%q", ok, why)
	}
}
