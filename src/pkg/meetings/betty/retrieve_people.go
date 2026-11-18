package betty

import (
	"context"
	"strings"
)

// PeopleSource is a Retriever source name.
const PeopleSource = "people"

// PersonBrief is what the persona may say about a customer: a name, the
// tags that are on and the goals a person confirmed. It deliberately has no
// field for notes, a satisfaction assessment, an address or an id: the
// persona speaks in a room that can hold people who are not the hive's
// users, so what is not here cannot be said.
type PersonBrief struct {
	Name  string
	Tags  []string
	Goals []string
}

// PeopleDirectory finds the customers a question names. The meetings
// package does not import the customers package; cmd/hive supplies this.
type PeopleDirectory interface {
	Find(terms []string, limit int) []PersonBrief
}

// PeopleRetriever answers "what does Ada want?" from the customers the
// hive keeps. Directory is read at call time, so it may be wired after the
// persona and may be nil while customers are off.
type PeopleRetriever struct {
	Directory func() PeopleDirectory
}

// Name implements Retriever.
func (PeopleRetriever) Name() string { return PeopleSource }

// Retrieve implements Retriever. Only the question's own words are used to
// look people up; nothing is guessed from the meeting.
func (p PeopleRetriever) Retrieve(ctx context.Context, _ string, terms []string, limit int) []Snippet {
	if p.Directory == nil || len(terms) == 0 || ctx.Err() != nil {
		return nil
	}
	dir := p.Directory()
	if dir == nil {
		return nil
	}
	var out []Snippet
	for _, b := range dir.Find(terms, limit) {
		if len(out) >= limit || strings.TrimSpace(b.Name) == "" {
			break
		}
		out = append(out, Snippet{Source: PeopleSource, Title: b.Name, Text: peopleText(b)})
	}
	return out
}

func peopleText(b PersonBrief) string {
	var parts []string
	if len(b.Tags) > 0 {
		parts = append(parts, "Known as: "+strings.Join(b.Tags, ", ")+".")
	}
	if len(b.Goals) > 0 {
		parts = append(parts, "What they are trying to get done: "+strings.Join(b.Goals, "; ")+".")
	}
	if len(parts) == 0 {
		parts = append(parts, "A customer of this hive; nothing more is recorded.")
	}
	text := strings.Join(parts, " ")
	if r := []rune(text); len(r) > maxSnippetRunes {
		text = string(r[:maxSnippetRunes-1]) + "…"
	}
	return text
}
