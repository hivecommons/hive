package customers

import (
	"errors"
	"fmt"
	"strings"
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
	OpName: true, OpIdentityAdd: true, OpIdentityRemove: true, OpTag: true,
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
	Hints []Hint `json:"hints,omitempty"`
	// Hinted reports that the last identity edit was not linked because
	// the identity belongs to someone else, and was raised as a question
	// instead.
	Hinted bool `json:"hinted,omitempty"`
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
	if !v.Full {
		// A viewer without the full view sees what they may edit. Notes,
		// the satisfaction assessment, merge history and hints are the
		// maintainers' working material and can name other people.
		out.Notes = ""
		out.Satisfaction = Satisfaction{}
		out.MergedFrom = nil
		out.RejectedGoals = nil
		return out
	}
	if detail {
		out.Hints = s.store.Hints(p.ID)
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

// Dismiss turns a suggestion down. Full view only.
func (s *Service) Dismiss(v Viewer, suggestionID string) error {
	if !v.Full {
		return ErrForbidden
	}
	return s.store.Dismiss(suggestionID)
}
