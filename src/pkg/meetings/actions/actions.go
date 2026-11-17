// Package actions is the approval queue for items people ask the meeting
// persona to draft (hivecommons/hive#18406): an issue, an RFC, a Spek
// session or a knowledge note. Speaker identity in a meeting is only a
// display name, so a draft never creates anything by itself: it waits until
// an owner or read-write user approves, edits or rejects it in the
// dashboard. Only knowledge notes may be auto-approved, and only when
// meetings.persona.actions.auto_approve.knowledge is on.
//
// On approval the draft goes to its Executor (the GitHub issue path for
// issues and RFCs, the Inbox spek_rfc destination for Spek sessions, the
// meetings knowledge vault for notes) and the result is announced back to
// the meeting: in its chat while it is live, else in its notes.
package actions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Type is one action type.
type Type string

// Action types; the values are config.MeetingsAction*.
const (
	TypeIssue     Type = config.MeetingsActionIssue
	TypeRFC       Type = config.MeetingsActionRFC
	TypeSpek      Type = config.MeetingsActionSpek
	TypeKnowledge Type = config.MeetingsActionKnowledge
)

// ParseType reads a type name.
func ParseType(s string) (Type, bool) {
	switch t := Type(strings.ToLower(strings.TrimSpace(s))); t {
	case TypeIssue, TypeRFC, TypeSpek, TypeKnowledge:
		return t, true
	}
	return "", false
}

// Noun is the type as people say it ("issue", "RFC").
func (t Type) Noun() string {
	switch t {
	case TypeIssue:
		return "issue"
	case TypeRFC:
		return "RFC"
	case TypeSpek:
		return "Spek session"
	case TypeKnowledge:
		return "knowledge note"
	}
	return string(t)
}

// WithArticle is the noun with "a" or "an".
func (t Type) WithArticle() string {
	switch t {
	case TypeIssue, TypeRFC:
		return "an " + t.Noun()
	}
	return "a " + t.Noun()
}

// NeedsRepo reports whether the type targets a repository.
func (t Type) NeedsRepo() bool { return t != TypeKnowledge }

// Status is a draft's lifecycle.
type Status string

// Draft statuses.
const (
	StatusPending   Status = "pending"
	StatusApproving Status = "approving"
	StatusDone      Status = "done"
	StatusFailed    Status = "failed"
	StatusRejected  Status = "rejected"
)

// Draft limits and retention.
const (
	MaxTitleRunes   = 200
	MaxBodyRunes    = 4000
	MaxExcerptRunes = 1200
	maxRequestRunes = 500
	// keepDecided bounds decided drafts kept on file.
	keepDecided = 200
	// decidedRetention drops decided drafts older than this.
	decidedRetention = 30 * 24 * time.Hour
	// DefaultExecuteTimeout bounds one executor call.
	DefaultExecuteTimeout = 90 * time.Second
	// FileName is the queue file in the meetings data directory.
	FileName = "actions.json"
	fileMode = 0o600
	dirMode  = 0o700
	// AutoApprover is DecidedBy for an auto-approved knowledge note.
	AutoApprover = "auto-approve (meetings.persona.actions.auto_approve.knowledge)"
)

// Errors. The dashboard maps them to 404, 400, 409 and 503.
var (
	ErrNotFound = errors.New("meeting draft not found")
	ErrInvalid  = errors.New("invalid meeting draft")
	ErrConflict = errors.New("meeting draft is not in a state that allows this")
	ErrDisabled = errors.New("meeting action requests are turned off (meetings.persona.actions)")
)

// Draft is one requested item.
type Draft struct {
	ID           string `json:"id"`
	MeetingID    string `json:"meeting_id"`
	MeetingTitle string `json:"meeting_title,omitempty"`
	// MeetingLink is the dashboard link to the meeting's panel.
	MeetingLink string `json:"meeting_link,omitempty"`
	Type        Type   `json:"type"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	// Repo is owner/repo for issues, RFCs and Spek sessions.
	Repo string `json:"repo,omitempty"`
	// Excerpt is the transcript around the request: meeting speech, data.
	Excerpt string `json:"excerpt,omitempty"`
	// RequestedBy is the speaker's display name (unverified).
	RequestedBy string `json:"requested_by,omitempty"`
	// Request is the line that asked for the draft.
	Request string `json:"request,omitempty"`
	Spoken  bool   `json:"spoken,omitempty"`

	Status       Status `json:"status"`
	AutoApproved bool   `json:"auto_approved,omitempty"`
	ResultURL    string `json:"result_url,omitempty"`
	ResultNote   string `json:"result_note,omitempty"`
	Error        string `json:"error,omitempty"`
	// Announced is where the result was reported: "chat" or "notes".
	Announced string     `json:"announced,omitempty"`
	EditedBy  string     `json:"edited_by,omitempty"`
	DecidedBy string     `json:"decided_by,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Edit changes a pending draft; nil fields are kept.
type Edit struct {
	Type  *string
	Title *string
	Body  *string
	Repo  *string
}

// Settings are the effective action settings.
type Settings struct {
	Enabled bool
	Types   []Type
	// DefaultRepo is where a draft goes when its repo is not a monitored
	// one.
	DefaultRepo string
	// Repos are the monitored repos a draft may target.
	Repos       []string
	AutoApprove map[Type]bool
}

// SettingsFrom converts the resolved config.
func SettingsFrom(r config.ResolvedMeetingsActions, defaultRepo string, repos []string) Settings {
	s := Settings{Enabled: r.Enabled, DefaultRepo: defaultRepo, Repos: repos, AutoApprove: map[Type]bool{}}
	for _, t := range r.Types {
		if tt, ok := ParseType(t); ok {
			s.Types = append(s.Types, tt)
		}
	}
	for t, on := range r.AutoApprove {
		if tt, ok := ParseType(t); ok {
			s.AutoApprove[tt] = on
		}
	}
	return s
}

// Allows reports whether a type is on.
func (s Settings) Allows(t Type) bool {
	if !s.Enabled {
		return false
	}
	for _, x := range s.Types {
		if x == t {
			return true
		}
	}
	return false
}

// AutoApproves reports whether a type is created without approval. Only
// knowledge notes ever are, whatever the settings say.
func (s Settings) AutoApproves(t Type) bool {
	return t == TypeKnowledge && s.AutoApprove[t]
}

// ResolveRepo is the repo a draft of type t targets: want when it is a
// monitored repo (case-insensitive), else the default repo, else the first
// monitored repo.
func (s Settings) ResolveRepo(t Type, want string) string {
	if !t.NeedsRepo() {
		return ""
	}
	want = strings.Trim(strings.TrimSpace(want), "/")
	for _, r := range s.Repos {
		if want != "" && strings.EqualFold(r, want) {
			return r
		}
	}
	if s.DefaultRepo != "" {
		return s.DefaultRepo
	}
	if len(s.Repos) > 0 {
		return s.Repos[0]
	}
	return ""
}

// knownRepo returns repo as configured when it is the default or a
// monitored repo.
func (s Settings) knownRepo(repo string) (string, bool) {
	if repo == "" {
		return "", false
	}
	if strings.EqualFold(repo, s.DefaultRepo) {
		return s.DefaultRepo, true
	}
	for _, r := range s.Repos {
		if strings.EqualFold(r, repo) {
			return r, true
		}
	}
	return "", false
}

// Result is what an executor created.
type Result struct {
	URL  string
	Note string
}

// Executor creates one approved draft.
type Executor interface {
	Execute(ctx context.Context, d Draft) (Result, error)
}

// ExecutorFunc adapts a function.
type ExecutorFunc func(ctx context.Context, d Draft) (Result, error)

// Execute implements Executor.
func (f ExecutorFunc) Execute(ctx context.Context, d Draft) (Result, error) { return f(ctx, d) }

// Announcer reports a result back to a meeting. It returns where the text
// went ("chat" or "notes").
type Announcer interface {
	Announce(ctx context.Context, meetingID, text string) (string, error)
}

// Options configure a Service.
type Options struct {
	// Dir holds actions.json (the meetings data directory).
	Dir       string
	Executors map[Type]Executor
	// Settings returns the current settings; it is read on every call so
	// a saved change applies at once.
	Settings func() Settings
	Announce Announcer
	Now      func() time.Time
	Logger   *slog.Logger
	Timeout  time.Duration
	// NewID mints draft ids; nil uses random hex.
	NewID func() string
}

// Service is the approval queue. Safe for concurrent use.
type Service struct {
	opts Options
	path string
	log  *slog.Logger

	mu     sync.Mutex
	drafts map[string]Draft
	// announcer may be set after the live runner starts.
	announcer Announcer
	wg        sync.WaitGroup
}

type storeFile struct {
	Version int     `json:"version"`
	Drafts  []Draft `json:"drafts"`
}

const fileVersion = 1

// interruptedError is the error a draft gets when a restart cut off its
// approval.
const interruptedError = "approval was interrupted by a restart; check the target before approving again"

// New opens <dir>/actions.json. A missing file is an empty queue; an
// unreadable one is an error so a damaged queue is never replaced.
func New(opts Options) (*Service, error) {
	if opts.Settings == nil {
		return nil, errors.New("actions: settings are required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultExecuteTimeout
	}
	if opts.NewID == nil {
		opts.NewID = randomID
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{opts: opts, log: log, drafts: map[string]Draft{}, announcer: opts.Announce}
	if opts.Dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(opts.Dir, dirMode); err != nil {
		return nil, fmt.Errorf("actions: creating dir: %w", err)
	}
	s.path = filepath.Join(opts.Dir, FileName)
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("actions: reading %s: %w", FileName, err)
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("actions: decoding %s: %w", FileName, err)
	}
	changed := false
	for _, d := range f.Drafts {
		if d.ID == "" {
			continue
		}
		if d.Status == StatusApproving {
			// The executor may or may not have created the item; never
			// retry it on our own.
			d.Status, d.Error, d.UpdatedAt = StatusFailed, interruptedError, opts.Now()
			changed = true
		}
		s.drafts[d.ID] = d
	}
	if changed {
		if err := s.saveLocked(); err != nil {
			log.Warn("[meetings] saving meeting drafts after restart", "error", err)
		}
	}
	return s, nil
}

// SetAnnouncer installs the announcer (the live runner).
func (s *Service) SetAnnouncer(a Announcer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.announcer = a
}

// Settings returns the current settings.
func (s *Service) Settings() Settings { return s.opts.Settings() }

// Wait blocks until background approvals finish (tests, shutdown).
func (s *Service) Wait() { s.wg.Wait() }

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("act_%d", time.Now().UnixNano())
	}
	return "act_" + hex.EncodeToString(b)
}

func (s *Service) saveLocked() error {
	if s.path == "" {
		return nil
	}
	s.pruneLocked()
	list := s.sortedLocked()
	data, err := json.MarshalIndent(storeFile{Version: fileVersion, Drafts: list}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.path, data)
}

func (s *Service) sortedLocked() []Draft {
	out := make([]Draft, 0, len(s.drafts))
	for _, d := range s.drafts {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// pruneLocked drops decided drafts past the retention or the cap.
func (s *Service) pruneLocked() {
	cut := s.opts.Now().Add(-decidedRetention)
	kept := 0
	for _, d := range s.sortedLocked() {
		if d.Status == StatusPending || d.Status == StatusApproving || d.Status == StatusFailed {
			continue
		}
		kept++
		if kept > keepDecided || d.UpdatedAt.Before(cut) {
			delete(s.drafts, d.ID)
		}
	}
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".actions-*")
	if err != nil {
		return fmt.Errorf("actions: temp file: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, fileMode)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("actions: writing %s: %w", FileName, werr)
	}
	return nil
}

// mentionRe matches an @handle, which would notify a GitHub user.
var mentionRe = regexp.MustCompile(`(^|[^\w@])@([A-Za-z0-9][A-Za-z0-9-]*)`)

// Defang keeps meeting text from mentioning GitHub users when it is filed:
// "@name" becomes "name". Meeting speech is data, so it never pings anyone.
func Defang(s string) string { return mentionRe.ReplaceAllString(s, "$1$2") }

func clipRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// oneLine collapses whitespace.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clean bounds and defangs a draft's text fields.
func clean(d *Draft) {
	d.Title = clipRunes(Defang(oneLine(d.Title)), MaxTitleRunes)
	d.Body = clipRunes(Defang(d.Body), MaxBodyRunes)
	d.Excerpt = clipRunes(Defang(d.Excerpt), MaxExcerptRunes)
	d.Request = clipRunes(Defang(oneLine(d.Request)), maxRequestRunes)
	d.RequestedBy = clipRunes(oneLine(d.RequestedBy), MaxTitleRunes)
}

// Submit queues a draft. Its repo is resolved against the monitored repos
// (never taken from meeting text unchecked). A knowledge note is approved
// at once when auto-approve is on; everything else waits.
func (s *Service) Submit(d Draft) (Draft, error) {
	st := s.Settings()
	if !st.Enabled {
		return Draft{}, ErrDisabled
	}
	if !st.Allows(d.Type) {
		return Draft{}, fmt.Errorf("%w: %s drafts are turned off", ErrDisabled, d.Type.Noun())
	}
	if strings.TrimSpace(d.MeetingID) == "" {
		return Draft{}, fmt.Errorf("%w: meeting id is required", ErrInvalid)
	}
	clean(&d)
	if d.Title == "" {
		return Draft{}, fmt.Errorf("%w: title is required", ErrInvalid)
	}
	d.Repo = st.ResolveRepo(d.Type, d.Repo)
	now := s.opts.Now()
	d.ID = s.opts.NewID()
	d.Status, d.CreatedAt, d.UpdatedAt = StatusPending, now, now
	d.ResultURL, d.ResultNote, d.Error, d.Announced, d.DecidedBy, d.DecidedAt, d.EditedBy = "", "", "", "", "", nil, ""
	d.AutoApproved = false
	auto := st.AutoApproves(d.Type)
	s.mu.Lock()
	if _, dup := s.drafts[d.ID]; dup {
		s.mu.Unlock()
		return Draft{}, fmt.Errorf("%w: duplicate draft id", ErrConflict)
	}
	if auto {
		d.AutoApproved = true
		d.Status = StatusApproving
		d.DecidedBy = AutoApprover
		at := now
		d.DecidedAt = &at
	}
	s.drafts[d.ID] = d
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		s.log.Warn("[meetings] saving meeting draft", "error", err)
	}
	if auto {
		s.wg.Add(1)
		go func(d Draft) {
			defer s.wg.Done()
			s.execute(context.Background(), d)
		}(d)
	}
	return d, nil
}

// List returns every draft, newest first.
func (s *Service) List() []Draft {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedLocked()
}

// Get returns one draft.
func (s *Service) Get(id string) (Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drafts[id]
	if !ok {
		return Draft{}, ErrNotFound
	}
	return d, nil
}

// editable reports whether a draft may be edited, approved or rejected.
func editable(st Status) bool { return st == StatusPending || st == StatusFailed }

// Edit changes a pending (or failed) draft's type, title, body or repo.
func (s *Service) Edit(id string, e Edit, by string) (Draft, error) {
	st := s.Settings()
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drafts[id]
	if !ok {
		return Draft{}, ErrNotFound
	}
	if !editable(d.Status) {
		return Draft{}, fmt.Errorf("%w: it is %s", ErrConflict, d.Status)
	}
	if e.Type != nil {
		t, ok := ParseType(*e.Type)
		if !ok {
			return Draft{}, fmt.Errorf("%w: unknown type %q", ErrInvalid, *e.Type)
		}
		if !st.Allows(t) {
			return Draft{}, fmt.Errorf("%w: %s drafts are turned off", ErrInvalid, t.Noun())
		}
		d.Type = t
	}
	if e.Title != nil {
		d.Title = *e.Title
	}
	if e.Body != nil {
		d.Body = *e.Body
	}
	clean(&d)
	if d.Title == "" {
		return Draft{}, fmt.Errorf("%w: title is required", ErrInvalid)
	}
	switch {
	case !d.Type.NeedsRepo():
		d.Repo = ""
	case e.Repo != nil:
		repo, ok := st.knownRepo(strings.Trim(strings.TrimSpace(*e.Repo), "/"))
		if !ok {
			return Draft{}, fmt.Errorf("%w: %q is not a monitored repo", ErrInvalid, strings.TrimSpace(*e.Repo))
		}
		d.Repo = repo
	case d.Repo == "":
		d.Repo = st.ResolveRepo(d.Type, "")
	}
	d.EditedBy, d.UpdatedAt = strings.TrimSpace(by), s.opts.Now()
	s.drafts[id] = d
	if err := s.saveLocked(); err != nil {
		return Draft{}, err
	}
	return d, nil
}

// Reject drops a pending draft; nothing is created.
func (s *Service) Reject(id, by string) (Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.drafts[id]
	if !ok {
		return Draft{}, ErrNotFound
	}
	if !editable(d.Status) {
		return Draft{}, fmt.Errorf("%w: it is %s", ErrConflict, d.Status)
	}
	now := s.opts.Now()
	d.Status, d.DecidedBy, d.DecidedAt, d.UpdatedAt = StatusRejected, strings.TrimSpace(by), &now, now
	s.drafts[id] = d
	if err := s.saveLocked(); err != nil {
		return Draft{}, err
	}
	return d, nil
}

// Approve creates a pending (or failed) draft through its executor and
// reports the result to the meeting. It returns the draft as it ended.
func (s *Service) Approve(ctx context.Context, id, by string) (Draft, error) {
	if !s.Settings().Enabled {
		return Draft{}, ErrDisabled
	}
	s.mu.Lock()
	d, ok := s.drafts[id]
	if !ok {
		s.mu.Unlock()
		return Draft{}, ErrNotFound
	}
	if !editable(d.Status) {
		s.mu.Unlock()
		return Draft{}, fmt.Errorf("%w: it is %s", ErrConflict, d.Status)
	}
	if d.Type.NeedsRepo() && d.Repo == "" {
		s.mu.Unlock()
		return Draft{}, fmt.Errorf("%w: no repo: set meetings.persona.actions.default_repo or edit the draft", ErrInvalid)
	}
	now := s.opts.Now()
	d.Status, d.DecidedBy, d.DecidedAt, d.UpdatedAt, d.Error = StatusApproving, strings.TrimSpace(by), &now, now, ""
	s.drafts[id] = d
	if err := s.saveLocked(); err != nil {
		s.log.Warn("[meetings] saving meeting draft", "error", err)
	}
	s.mu.Unlock()
	return s.execute(ctx, d), nil
}

// execute runs an approving draft and records the result.
func (s *Service) execute(ctx context.Context, d Draft) Draft {
	ex := s.opts.Executors[d.Type]
	var (
		res Result
		err error
	)
	if ex == nil {
		err = fmt.Errorf("%s drafts cannot be created on this hive", d.Type.Noun())
	} else {
		cctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
		res, err = ex.Execute(cctx, d)
		cancel()
	}
	s.mu.Lock()
	cur, ok := s.drafts[d.ID]
	if !ok {
		cur = d
	}
	cur.UpdatedAt = s.opts.Now()
	if err != nil {
		cur.Status, cur.Error = StatusFailed, err.Error()
		s.log.Warn("[meetings] meeting draft not created", "draft", d.ID, "type", d.Type, "error", err)
	} else {
		cur.Status, cur.ResultURL, cur.ResultNote, cur.Error = StatusDone, res.URL, res.Note, ""
	}
	s.drafts[d.ID] = cur
	if serr := s.saveLocked(); serr != nil {
		s.log.Warn("[meetings] saving meeting draft", "error", serr)
	}
	ann := s.announcer
	s.mu.Unlock()
	if err != nil || ann == nil {
		return cur
	}
	where, aerr := ann.Announce(ctx, cur.MeetingID, ResultText(cur))
	if aerr != nil {
		s.log.Info("[meetings] meeting draft result not announced", "draft", d.ID, "error", aerr)
		return cur
	}
	s.mu.Lock()
	if x, ok := s.drafts[d.ID]; ok {
		x.Announced = where
		s.drafts[d.ID] = x
		cur = x
		if serr := s.saveLocked(); serr != nil {
			s.log.Warn("[meetings] saving meeting draft", "error", serr)
		}
	}
	s.mu.Unlock()
	return cur
}

// ResultText is what the meeting is told once a draft was created.
func ResultText(d Draft) string {
	title := clipRunes(d.Title, maxAnnouncedTitle)
	switch d.Type {
	case TypeKnowledge:
		if d.ResultURL != "" {
			return fmt.Sprintf("Saved the knowledge note %q to the hive: %s", title, d.ResultURL)
		}
		return fmt.Sprintf("Saved the knowledge note %q to the hive.", title)
	case TypeSpek:
		if d.ResultURL != "" {
			return fmt.Sprintf("Approved: the Spek session on %q started: %s", title, d.ResultURL)
		}
		note := d.ResultNote
		if note == "" {
			note = "handed to a hive agent"
		}
		return fmt.Sprintf("Approved: the Spek session on %q was %s.", title, strings.TrimSuffix(note, "."))
	default:
		if d.ResultURL != "" {
			return fmt.Sprintf("Approved: %s %q is open: %s", d.Type.WithArticle(), title, d.ResultURL)
		}
		return fmt.Sprintf("Approved: %s %q was created.", d.Type.WithArticle(), title)
	}
}

// maxAnnouncedTitle bounds the title quoted in a meeting post.
const maxAnnouncedTitle = 120

// RenderBody is the body of a created item (issue, RFC, note): the draft
// body, then where it came from. The excerpt is meeting speech, quoted in
// a code block so it renders as text.
func RenderBody(d Draft) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(d.Body))
	b.WriteString("\n\n---\n\n")
	label := d.MeetingTitle
	if label == "" {
		label = "meeting " + d.MeetingID
	}
	if d.MeetingLink != "" {
		fmt.Fprintf(&b, "Drafted by the hive's meeting attendee during [%s](%s)", label, d.MeetingLink)
	} else {
		fmt.Fprintf(&b, "Drafted by the hive's meeting attendee during %s", label)
	}
	if d.RequestedBy != "" {
		fmt.Fprintf(&b, ", asked for by %s (meeting display name)", d.RequestedBy)
	}
	b.WriteString(". ")
	if d.AutoApproved {
		b.WriteString("Auto-approved.")
	} else if d.DecidedBy != "" {
		fmt.Fprintf(&b, "Approved in the dashboard by %s.", d.DecidedBy)
	}
	b.WriteString("\n")
	if ex := strings.TrimSpace(d.Excerpt); ex != "" {
		b.WriteString("\nTranscript excerpt:\n\n```text\n")
		b.WriteString(strings.ReplaceAll(ex, "```", "'''"))
		b.WriteString("\n```\n")
	}
	return b.String()
}

// MeetingSink is the queue as one meeting's persona sees it: drafts it
// submits carry that meeting's id, title and dashboard link.
type MeetingSink struct {
	svc   *Service
	id    string
	title string
	link  string
}

// ForMeeting returns the sink for one meeting.
func (s *Service) ForMeeting(id, title, link string) *MeetingSink {
	return &MeetingSink{svc: s, id: id, title: title, link: link}
}

// Settings implements the persona's sink.
func (m *MeetingSink) Settings() Settings { return m.svc.Settings() }

// Submit implements the persona's sink.
func (m *MeetingSink) Submit(d Draft) (Draft, error) {
	d.MeetingID, d.MeetingTitle, d.MeetingLink = m.id, m.title, m.link
	return m.svc.Submit(d)
}
