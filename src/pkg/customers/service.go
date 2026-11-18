package customers

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrForbidden is returned when the viewer may not do what was asked. A
// viewer asking for a profile they may not see gets ErrNotFound instead, so
// the answer does not reveal that the profile exists.
var ErrForbidden = errors.New("customers: forbidden")

// MeID is the id alias for the viewer's own profile.
const MeID = "me"

// Edit operations.
const (
	OpName                = "name"
	OpNotes               = "notes"
	OpProfile             = "profile"
	OpAffiliationDismiss  = "affiliation_dismiss"
	OpIdentityAdd         = "identity_add"
	OpIdentityRemove      = "identity_remove"
	OpTag                 = "tag"
	OpGoalAdd             = "goal_add"
	OpGoalEdit            = "goal_edit"
	OpGoalConfirm         = "goal_confirm"
	OpGoalReject          = "goal_reject"
	OpSatisfactionSet     = "satisfaction_set"
	OpSatisfactionConfirm = "satisfaction_confirm"
	OpSatisfactionReject  = "satisfaction_reject"
)

// Tag states an edit may ask for. TagAuto hands the tag back to the rules.
const TagAuto = "auto"

// selfOps is what a viewer without the full view may do, and only to their
// own profile: name, identities, tags and goals.
var selfOps = map[string]bool{
	OpName: true, OpProfile: true, OpAffiliationDismiss: true, OpIdentityAdd: true, OpIdentityRemove: true, OpTag: true,
	OpGoalAdd: true, OpGoalEdit: true, OpGoalConfirm: true, OpGoalReject: true,
}

// Edit is one change to a profile. Which fields matter depends on Op.
type Edit struct {
	Op    string
	Name  string
	Notes string
	// Kind and Value name an identity to add; Key ("kind:value") names one
	// to remove.
	Kind  string
	Value string
	Key   string
	// Tag and State ("on", "off", "auto") set a tag.
	Tag   string
	State string
	// GoalID and Text address a goal.
	GoalID string
	Text   string
	// Field names a profile field (Value is its text; State "auto" hands
	// it back to the automatic source).
	Field string
	// Level sets satisfaction.
	Level string
}

// PersonView is a profile as one viewer sees it.
type PersonView struct {
	Person
	// Me is true on the viewer's own profile.
	Me bool `json:"me"`
	// Counts is the person's edges by predicate.
	Counts map[string]int `json:"counts,omitempty"`
	// Hints are the open "Same person?" questions. Full view only: a hint
	// names another profile.
	Hints []HintView `json:"hints,omitempty"`
	// Hinted reports that the last identity edit was not linked because
	// the identity belongs to someone else, and was raised as a question
	// instead.
	Hinted bool `json:"hinted,omitempty"`
	// Affiliation is a suggested organization with its evidence, never a
	// stored fact: Accept writes it as a by-hand organization. Detail views
	// only.
	Affiliation *AffiliationSuggestion `json:"affiliation_suggestion,omitempty"`
}

// ListView is the Customers list as one viewer sees it.
type ListView struct {
	Me PersonView `json:"me"`
	// Full is false for a viewer who sees only their own profile.
	Full        bool         `json:"full"`
	Owner       bool         `json:"owner"`
	People      []PersonView `json:"people"`
	Suggestions []Suggestion `json:"suggestions"`
	DefaultTags []string     `json:"default_tags"`
	Levels      []Level      `json:"levels"`
}

// Service is the viewer-aware face of the store. Every method takes the
// viewer and enforces the access rules itself, whatever the caller already
// checked: a viewer without the full view reaches only their own profile,
// by any method.
type Service struct {
	store     *Store
	index     *Index
	resolvers Resolvers
}

// NewService wraps a store. index and resolvers may be nil.
func NewService(store *Store, index *Index, resolvers Resolvers) *Service {
	return &Service{store: store, index: index, resolvers: resolvers}
}

// Store is the underlying people store.
func (s *Service) Store() *Store { return s.store }

// Index is the edge index, or nil.
func (s *Service) Index() *Index { return s.index }

// Resolvers answers whether artefacts exist and who may see them.
func (s *Service) Resolvers() Resolvers { return s.resolvers }

// me returns the viewer's own profile, creating it on first use. A viewer
// with no name has no profile.
func (s *Service) me(v Viewer) (Person, error) {
	if strings.TrimSpace(v.User) == "" {
		return Person{}, ErrForbidden
	}
	return s.store.EnsureHiveUser(v.User)
}

// resolve finds the profile an id names for this viewer. Without the full
// view, any id but the viewer's own answers ErrNotFound, exactly as an id
// that does not exist would.
func (s *Service) resolve(v Viewer, id string) (Person, bool, error) {
	me, err := s.me(v)
	if err != nil {
		return Person{}, false, err
	}
	if id == MeID || id == me.ID {
		return me, true, nil
	}
	if !v.Full {
		return Person{}, false, ErrNotFound
	}
	p, err := s.store.Get(id)
	return p, false, err
}

// CanSee reports whether the viewer may see the profile with this id. It is
// the single check every read path (REST and graph) goes through.
func (s *Service) CanSee(v Viewer, id string) bool {
	_, _, err := s.resolve(v, id)
	return err == nil
}

// view builds what the viewer sees of p.
func (s *Service) view(v Viewer, p Person, isMe, detail bool) PersonView {
	out := PersonView{Person: p, Me: isMe}
	if detail {
		out.Affiliation = SuggestAffiliation(p)
	}
	if !v.Full {
		// A viewer without the full view sees what they may edit. Notes,
		// the satisfaction assessment, merge history and hints are the
		// maintainers' working material and can name other people.
		out.Notes = ""
		out.Satisfaction = Satisfaction{}
		out.MergedFrom = nil
		out.RejectedGoals = nil
		out.InferredAt, out.InferredHash = time.Time{}, ""
		return out
	}
	if detail {
		for _, h := range s.store.Hints(p.ID) {
			out.Hints = append(out.Hints, s.hintView(h))
		}
		if s.index != nil {
			if counts, err := s.index.Counts(p.ID); err == nil && len(counts) > 0 {
				out.Counts = counts
			}
		}
	}
	return out
}

// List returns the viewer's own profile and, with the full view, every
// customer and suggestion.
func (s *Service) List(v Viewer) (ListView, error) {
	me, err := s.me(v)
	if err != nil {
		return ListView{}, err
	}
	out := ListView{
		Me: s.view(v, me, true, false), Full: v.Full, Owner: v.Full && v.Owner,
		People: []PersonView{}, Suggestions: []Suggestion{},
		DefaultTags: DefaultTags, Levels: Levels,
	}
	if !v.Full {
		return out, nil
	}
	for _, p := range s.store.People() {
		if p.ID == me.ID {
			continue
		}
		out.People = append(out.People, s.view(v, p, false, false))
	}
	out.Suggestions = s.store.Suggestions()
	return out, nil
}

// Get returns one profile. id may be MeID.
func (s *Service) Get(v Viewer, id string) (PersonView, error) {
	p, isMe, err := s.resolve(v, id)
	if err != nil {
		return PersonView{}, err
	}
	return s.view(v, p, isMe, true), nil
}

// Add creates a customer by hand. Full view only.
func (s *Service) Add(v Viewer, name string, ids []Identity) (PersonView, error) {
	if !v.Full {
		return PersonView{}, ErrForbidden
	}
	p, err := s.store.Add(name, ids, v.User)
	if err != nil {
		return PersonView{}, err
	}
	return s.view(v, p, false, true), nil
}

// Edit applies one change to a profile. Without the full view only the
// viewer's own profile can be edited, and only its name, identities, tags
// and goals.
func (s *Service) Edit(v Viewer, id string, e Edit) (PersonView, error) {
	p, isMe, err := s.resolve(v, id)
	if err != nil {
		return PersonView{}, err
	}
	if !v.Full && !selfOps[e.Op] {
		return PersonView{}, ErrForbidden
	}
	hinted := false
	switch e.Op {
	case OpName:
		p, err = s.store.SetName(p.ID, e.Name)
	case OpNotes:
		p, err = s.store.SetNotes(p.ID, e.Notes)
	case OpProfile:
		if e.State == TagAuto {
			p, err = s.store.ResetProfileField(p.ID, e.Field)
		} else {
			p, err = s.store.SetProfileField(p.ID, e.Field, e.Value, v.User)
		}
	case OpAffiliationDismiss:
		p, err = s.store.DismissAffiliation(p.ID, e.Value)
	case OpIdentityAdd:
		id := Identity{Kind: IdentityKind(e.Kind), Value: e.Value}
		if !v.Full {
			// A read-only user's word is a claim, not a key, until
			// someone with the full view confirms it.
			p, err = s.store.AddClaim(p.ID, id, v.User)
			break
		}
		var hint *Hint
		p, hint, err = s.store.AddIdentity(p.ID, id, v.User)
		hinted = hint != nil
	case OpIdentityRemove:
		p, _, err = s.store.RemoveIdentity(p.ID, e.Key, v.User)
		if err == nil && s.index != nil {
			// The edges that identity produced go with it.
			_, err = s.index.DeleteWhere(p.ID, ViaAny([]string{e.Key}))
		}
	case OpTag:
		switch e.State {
		case TagOn, TagOff:
			p, err = s.store.SetTag(p.ID, e.Tag, e.State == TagOn, v.User)
		case TagAuto:
			p, err = s.store.ResetTag(p.ID, e.Tag, v.User)
		default:
			err = fmt.Errorf("%w: tag state must be on, off or auto", ErrInvalid)
		}
	case OpGoalAdd:
		p, err = s.store.AddGoal(p.ID, e.Text, v.User)
	case OpGoalEdit:
		p, err = s.store.EditGoal(p.ID, e.GoalID, e.Text, v.User)
	case OpGoalConfirm:
		p, err = s.store.ConfirmGoal(p.ID, e.GoalID, v.User)
	case OpGoalReject:
		p, err = s.store.RejectGoal(p.ID, e.GoalID)
	case OpSatisfactionSet:
		p, err = s.store.SetSatisfaction(p.ID, Level(e.Level), v.User)
	case OpSatisfactionConfirm:
		p, err = s.store.ConfirmSatisfaction(p.ID, v.User)
	case OpSatisfactionReject:
		p, err = s.store.RejectSatisfaction(p.ID)
	default:
		err = fmt.Errorf("%w: unknown edit %q", ErrInvalid, e.Op)
	}
	if err != nil {
		return PersonView{}, err
	}
	out := s.view(v, p, isMe, true)
	out.Hinted = hinted
	return out, nil
}

// Forget deletes a customer and every edge to them, leaving tombstones so
// discovery does not rebuild them. Owner only. It returns how many edges
// were removed.
func (s *Service) Forget(v Viewer, id string) (int, error) {
	if !v.Full || !v.Owner {
		return 0, ErrForbidden
	}
	p, _, err := s.resolve(v, id)
	if err != nil {
		return 0, err
	}
	if _, err := s.store.Forget(p.ID); err != nil {
		return 0, err
	}
	if s.index == nil {
		return 0, nil
	}
	return s.index.DeletePerson(p.ID)
}

// Accept turns a suggestion into a customer. Full view only.
func (s *Service) Accept(v Viewer, suggestionID string) (PersonView, error) {
	if !v.Full {
		return PersonView{}, ErrForbidden
	}
	p, err := s.store.Accept(suggestionID, v.User)
	if err != nil {
		return PersonView{}, err
	}
	return s.view(v, p, false, true), nil
}

// AcceptInto attaches a suggested person to an existing customer (id may be
// "me") instead of making them a new one. It reuses the merge: the
// suggestion is accepted as a customer and folded into the target, so the
// identity becomes a manual one of the target's, the activity it produced
// moves onto the target on the next sweep, nobody is created twice, and
// Unmerge, with the id the result names in its merge history, undoes it.
// Full view only: a read-only user cannot see suggestions, and adds their
// own identities as unconfirmed claims instead.
func (s *Service) AcceptInto(v Viewer, suggestionID, intoID string) (PersonView, error) {
	if !v.Full {
		return PersonView{}, ErrForbidden
	}
	dst, dstMe, err := s.resolve(v, intoID)
	if err != nil {
		return PersonView{}, err
	}
	if len(dst.Identities) >= MaxIdentities {
		return PersonView{}, fmt.Errorf("%w: too many identities", ErrInvalid)
	}
	p, err := s.store.Accept(suggestionID, v.User)
	if err != nil {
		return PersonView{}, err
	}
	merged, err := s.store.Merge(dst.ID, p.ID, v.User)
	if err != nil {
		return PersonView{}, err
	}
	return s.view(v, merged, dstMe, true), nil
}

// Dismiss turns a suggestion down. Full view only.
func (s *Service) Dismiss(v Viewer, suggestionID string) error {
	if !v.Full {
		return ErrForbidden
	}
	return s.store.Dismiss(suggestionID)
}

// ArtefactPerson is one person on an artefact, as the viewer sees them.
type ArtefactPerson struct {
	Predicate string
	Person    PersonView
}

// Edges returns a page of a person's edges, newest first, leaving out the
// ones whose artefact the viewer may not see where it lives. Without the
// full view only the viewer's own edges can be read. A page can come back
// shorter than limit when edges were left out; next is still the cursor for
// the following page.
func (s *Service) Edges(v Viewer, id, predicate, after string, limit int) ([]Edge, string, error) {
	p, _, err := s.resolve(v, id)
	if err != nil {
		return nil, "", err
	}
	if s.index == nil {
		return nil, "", nil
	}
	edges, next, err := s.index.ByPerson(p.ID, predicate, after, limit)
	if err != nil {
		return nil, "", err
	}
	visible := edges[:0:0]
	for _, e := range edges {
		if s.resolvers.Visible(e.Artefact, v) {
			visible = append(visible, e)
		}
	}
	return visible, next, nil
}

// ArtefactVisible reports whether the viewer may be told about an artefact
// reference at all: the reference is well formed, the viewer is a named
// user, and the artefact's own source lets them see it. It says nothing
// about whether the artefact exists.
func (s *Service) ArtefactVisible(v Viewer, ref string) bool {
	if !validRef(ref) || strings.TrimSpace(v.User) == "" {
		return false
	}
	return s.resolvers.Visible(ref, v)
}

// ArtefactPeople walks back from an artefact to the people on it that the
// viewer may see. Without the full view that is at most the viewer.
func (s *Service) ArtefactPeople(v Viewer, ref string, limit int) ([]ArtefactPerson, error) {
	if !s.ArtefactVisible(v, ref) {
		return nil, ErrNotFound
	}
	if s.index == nil {
		return nil, nil
	}
	edges, err := s.index.ByArtefact(ref)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxEdgePage {
		limit = MaxEdgePage
	}
	var out []ArtefactPerson
	for _, e := range edges {
		if len(out) == limit {
			break
		}
		p, isMe, err := s.resolve(v, e.Person)
		if err != nil {
			continue // not visible to this viewer, or gone
		}
		out = append(out, ArtefactPerson{Predicate: e.Predicate, Person: s.view(v, p, isMe, false)})
	}
	return out, nil
}

// HintView is a hint with what it asks about spelled out, so a person can
// answer it without opening another profile. For a suggested person that is
// the suggestion's identity; for a customer, their name.
type HintView struct {
	Hint
	OtherKind  string `json:"other_kind,omitempty"`
	OtherValue string `json:"other_value,omitempty"`
}

func (s *Service) hintView(h Hint) HintView {
	v := HintView{Hint: h}
	if sg, ok := s.store.SuggestionByID(h.Other); ok {
		v.OtherKind, v.OtherValue = string(sg.Kind), sg.Value
	} else if p, err := s.store.Get(h.Other); err == nil {
		v.OtherKind, v.OtherValue = "customer", p.Name
	}
	return v
}

// AnswerHint answers a "Same person?" question. Yes makes the suggested
// identity the customer's own; no closes the question and remembers it, so
// it is not asked again. Full view only. A hint between two customers is
// answered by merging them, which is a separate action.
func (s *Service) AnswerHint(v Viewer, hintID string, same bool) (PersonView, error) {
	if !v.Full {
		return PersonView{}, ErrForbidden
	}
	h, ok := s.store.HintByID(hintID)
	if !ok {
		return PersonView{}, ErrNotFound
	}
	if !same {
		if _, err := s.store.ResolveHint(hintID, false, v.User); err != nil {
			return PersonView{}, err
		}
		p, err := s.store.Get(h.Person)
		if err != nil {
			return PersonView{}, err
		}
		return s.view(v, p, false, true), nil
	}
	if _, isSuggestion := s.store.SuggestionByID(h.Other); !isSuggestion {
		return PersonView{}, fmt.Errorf("%w: merge the two customers instead", ErrInvalid)
	}
	p, err := s.store.AbsorbSuggestion(h.Person, h.Other, v.User)
	if err != nil {
		return PersonView{}, err
	}
	return s.view(v, p, false, true), nil
}

// Merge folds the customer src into dst: identities, tags and goals come
// across, and so do the activity edges. The merge is recorded on dst, so
// Unmerge can undo it exactly. Full view only.
func (s *Service) Merge(v Viewer, dstID, srcID string) (PersonView, error) {
	if !v.Full {
		return PersonView{}, ErrForbidden
	}
	dst, dstMe, err := s.resolve(v, dstID)
	if err != nil {
		return PersonView{}, err
	}
	src, _, err := s.resolve(v, srcID)
	if err != nil {
		return PersonView{}, err
	}
	merged, err := s.store.Merge(dst.ID, src.ID, v.User)
	if err != nil {
		return PersonView{}, err
	}
	if s.index != nil {
		if _, err := s.index.Move(src.ID, dst.ID, func(Edge) bool { return true }); err != nil {
			return PersonView{}, err
		}
	}
	return s.view(v, merged, dstMe, true), nil
}

// Unmerge undoes a merge: the customer comes back as it stood, and the edges
// the merged identities produced go back with it. Full view only.
func (s *Service) Unmerge(v Viewer, dstID, srcID string) (PersonView, error) {
	if !v.Full {
		return PersonView{}, ErrForbidden
	}
	dst, dstMe, err := s.resolve(v, dstID)
	if err != nil {
		return PersonView{}, err
	}
	back, restored, err := s.store.Unmerge(dst.ID, srcID)
	if err != nil {
		return PersonView{}, err
	}
	if s.index != nil {
		keys := make([]string, 0, len(restored.Identities))
		for _, i := range restored.Identities {
			keys = append(keys, i.Key())
		}
		if _, err := s.index.Move(dst.ID, restored.ID, ViaAny(keys)); err != nil {
			return PersonView{}, err
		}
	}
	return s.view(v, back, dstMe, true), nil
}

// Split moves the identities named by keys off a customer onto a new one,
// and the edges those identities produced move with them. It returns the
// new customer. Full view only.
func (s *Service) Split(v Viewer, id string, keys []string, name string) (PersonView, error) {
	if !v.Full {
		return PersonView{}, ErrForbidden
	}
	p, _, err := s.resolve(v, id)
	if err != nil {
		return PersonView{}, err
	}
	_, fresh, err := s.store.Split(p.ID, keys, name, v.User)
	if err != nil {
		return PersonView{}, err
	}
	if s.index != nil {
		if _, err := s.index.Move(p.ID, fresh.ID, ViaAny(keys)); err != nil {
			return PersonView{}, err
		}
	}
	return s.view(v, fresh, false, true), nil
}

// ExportMaxEdges bounds how many edges one export carries.
const ExportMaxEdges = 10000

// Export is everything the hive holds about one customer, for the owner.
type Export struct {
	Person Person `json:"person"`
	// Edges are references to artefacts, never their content.
	Edges []Edge `json:"edges"`
	// Truncated is true when the person has more edges than ExportMaxEdges.
	Truncated bool   `json:"truncated,omitempty"`
	Hints     []Hint `json:"hints,omitempty"`
}

// Export returns a customer and their edges. Owner only: it holds notes, the
// satisfaction assessment and the merge history.
func (s *Service) Export(v Viewer, id string) (Export, error) {
	if !v.Full || !v.Owner {
		return Export{}, ErrForbidden
	}
	p, _, err := s.resolve(v, id)
	if err != nil {
		return Export{}, err
	}
	out := Export{Person: p, Edges: []Edge{}, Hints: s.store.Hints(p.ID)}
	if s.index == nil {
		return out, nil
	}
	after := ""
	for len(out.Edges) < ExportMaxEdges {
		page, next, err := s.index.ByPerson(p.ID, "", after, MaxEdgePage)
		if err != nil {
			return Export{}, err
		}
		out.Edges = append(out.Edges, page...)
		if next == "" {
			return out, nil
		}
		after = next
	}
	out.Truncated = true
	return out, nil
}
