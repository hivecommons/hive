package workflowmodel

import (
	"sort"
	"strings"
)

// ReplyEffect is what one human-gate verb does when a person posts it on
// an item in state From (hivecommons/hive#17620, RFC phase 4 follow-up:
// "the change regenerates labels and commands"). The compiler derives it
// from the model: the transition out of From whose gate is released by the
// verb (Gate.Reply), whose Note names it, or whose event is labelled with
// it. A verb with no such transition is still a recognised reply — the
// decision is recorded — but moves nothing.
type ReplyEffect struct {
	// Verb is the reply as the model declares it ("approve analysis").
	Verb string `json:"verb"`
	From string `json:"from"`
	// To is the target state, "" when the model declares no transition
	// for the verb.
	To string `json:"to,omitempty"`
	// Event is the transition's `on` event id.
	Event string `json:"event,omitempty"`
	// Label is the label a command handler applies to fire the move: the
	// event's `labels has X` value when the event observes a label, else
	// the model's label entry for To, else "" (record only).
	Label string `json:"label,omitempty"`
}

// NormalizeReply is the comparison form of a verb: lower-cased, with runs
// of whitespace, underscores and hyphens collapsed to one hyphen, so
// `approve analysis`, `approve-analysis` and `Approve_Analysis` are one
// verb on the command line.
func NormalizeReply(verb string) string {
	f := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(verb)), func(r rune) bool {
		return r == ' ' || r == '\t' || r == '_' || r == '-'
	})
	return strings.Join(f, "-")
}

// RepliesForNumber returns the verbs the state carrying stage number n
// offers, in declaration order; nil when no state carries n or it offers
// none.
func (p *Profile) RepliesForNumber(n int) []string {
	if p == nil {
		return nil
	}
	sid, ok := p.StateOfNumber[n]
	if !ok {
		return nil
	}
	return append([]string(nil), p.Replies[sid]...)
}

// Reply resolves verb for an item in state (by id) to its effect; ok is
// false when the state offers no such verb. Matching uses NormalizeReply.
func (p *Profile) Reply(state, verb string) (ReplyEffect, bool) {
	if p == nil {
		return ReplyEffect{}, false
	}
	key := NormalizeReply(verb)
	if key == "" {
		return ReplyEffect{}, false
	}
	if e, ok := p.replyEffects[state][key]; ok {
		return e, true
	}
	// A declared reply the compiler could not pin to a transition is still
	// a recognised verb: record only.
	for _, v := range p.Replies[state] {
		if NormalizeReply(v) == key {
			return ReplyEffect{Verb: v, From: state}, true
		}
	}
	return ReplyEffect{}, false
}

// ReplyForNumber is Reply for the state carrying stage number n.
func (p *Profile) ReplyForNumber(n int, verb string) (ReplyEffect, bool) {
	if p == nil {
		return ReplyEffect{}, false
	}
	sid, ok := p.StateOfNumber[n]
	if !ok {
		return ReplyEffect{}, false
	}
	return p.Reply(sid, verb)
}

// ReplyVerbs is every distinct verb the profile declares, sorted, for
// command-line parsers that must recognise a verb before they know the
// item's state.
func (p *Profile) ReplyVerbs() []string {
	if p == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, verbs := range p.Replies {
		for _, v := range verbs {
			k := NormalizeReply(v)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// replies pins each declared verb to the transition it releases.
func (c *compiler) replies() {
	m := c.m
	gates := map[string]Gate{}
	for _, g := range m.Spec.Gates {
		gates[g.ID] = g
	}
	events := map[string]Event{}
	for _, e := range m.Spec.Events {
		events[e.ID] = e
	}
	labelOf := map[string]string{}
	for _, e := range m.Spec.Labels.Entries {
		if e.State != "" && e.Name != "" {
			if _, dup := labelOf[e.State]; !dup {
				labelOf[e.State] = e.Name
			}
		}
	}
	// match reports how well t answers key: 3 gate reply, 2 note, 1 event
	// label/id, 0 no match.
	match := func(t Transition, key string) int {
		if g, ok := gates[t.GateRef]; ok && t.GateRef != "" && NormalizeReply(g.Reply) == key {
			return 3
		}
		if NormalizeReply(t.Note) == key {
			return 2
		}
		if ev, ok := events[t.On]; ok && (NormalizeReply(ev.Label) == key || NormalizeReply(ev.ID) == key) {
			return 1
		}
		return 0
	}
	for sid, verbs := range c.p.Replies {
		for _, v := range verbs {
			key := NormalizeReply(v)
			if key == "" {
				continue
			}
			best, bestScore := Transition{}, 0
			for _, t := range m.Spec.Transitions {
				if t.From != sid {
					continue
				}
				if s := match(t, key); s > bestScore {
					best, bestScore = t, s
				}
			}
			if bestScore == 0 {
				continue
			}
			eff := ReplyEffect{Verb: v, From: sid, To: best.To, Event: best.On}
			if ev, ok := events[best.On]; ok && ev.Fact.Fact == "labels" && ev.Fact.Op == OpHas && ev.Fact.Value != "" {
				eff.Label = ev.Fact.Value
			} else if l, ok := labelOf[best.To]; ok {
				eff.Label = l
			}
			if c.p.replyEffects == nil {
				c.p.replyEffects = map[string]map[string]ReplyEffect{}
			}
			if c.p.replyEffects[sid] == nil {
				c.p.replyEffects[sid] = map[string]ReplyEffect{}
			}
			c.p.replyEffects[sid][key] = eff
		}
	}
}
