package tokens

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// BackendManagedAgents identifies usage collected from a Claude Managed Agents
// session. It is deliberately distinct from the Claude Code CLI backend: the
// hosted adapter receives vendor events and writes its own bounded session log.
const BackendManagedAgents = "managed-agents"

// DefaultManagedAgentsDailyBudgetCents is the hard per-spoke daily cap for the
// hosted prototype: USD 25.00. The adapter may lower it for a test or a more
// restrictive private spoke, but never raises it.
const DefaultManagedAgentsDailyBudgetCents int64 = 2500

// ErrManagedAgentsBudgetExceeded indicates that active reservations and
// observed spend leave no safe budget for another hosted session.
var ErrManagedAgentsBudgetExceeded = errors.New("managed agents daily budget exceeded")

// ManagedAgentsLedger is the durable metering boundary for hosted sessions. It
// deduplicates vendor event IDs before they enter the normal token collector and
// keeps cumulative list-cost usage per UTC day so a restart cannot reset the
// spoke budget.
type ManagedAgentsLedger struct {
	dir         string
	path        string
	budgetCents int64
	now         func() time.Time

	mu   sync.Mutex
	data managedAgentsLedgerData
}

type managedAgentsLedgerData struct {
	Days         map[string]managedAgentsLedgerDay   `json:"days"`
	Events       map[string]managedAgentsTokenEvent  `json:"events"`
	Reservations map[string]managedAgentsReservation `json:"reservations"`
}

type managedAgentsLedgerDay struct {
	SpendCents int64                              `json:"spend_cents"`
	Sessions   map[string]managedAgentsSessionUse `json:"sessions"`
}

type managedAgentsSessionUse struct {
	ListCostCents int64 `json:"list_cost_cents"`
	ActiveSeconds int64 `json:"active_seconds,omitempty"`
	WebSearches   int64 `json:"web_searches,omitempty"`
}

type managedAgentsReservation struct {
	Day        string `json:"day"`
	LimitCents int64  `json:"limit_cents"`
}

type managedAgentsTokenEvent struct {
	SessionID   string    `json:"session_id"`
	EventID     string    `json:"event_id"`
	Agent       string    `json:"agent"`
	Model       string    `json:"model"`
	Input       int64     `json:"input_tokens"`
	Output      int64     `json:"output_tokens"`
	CacheRead   int64     `json:"cache_read"`
	CacheCreate int64     `json:"cache_creation"`
	Timestamp   time.Time `json:"timestamp"`
}

// NewManagedAgentsLedger opens or creates the durable ledger under dir.
func NewManagedAgentsLedger(dir string, budgetCents int64) (*ManagedAgentsLedger, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("managed agents ledger directory is required")
	}
	if budgetCents == 0 {
		budgetCents = DefaultManagedAgentsDailyBudgetCents
	}
	if budgetCents < 1 || budgetCents > DefaultManagedAgentsDailyBudgetCents {
		return nil, fmt.Errorf("managed agents daily budget must be between 1 and %d cents", DefaultManagedAgentsDailyBudgetCents)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create managed agents ledger directory: %w", err)
	}
	l := &ManagedAgentsLedger{
		dir:         dir,
		path:        filepath.Join(dir, "managed-agents-ledger.json"),
		budgetCents: budgetCents,
		now:         time.Now,
		data: managedAgentsLedgerData{
			Days:         map[string]managedAgentsLedgerDay{},
			Events:       map[string]managedAgentsTokenEvent{},
			Reservations: map[string]managedAgentsReservation{},
		},
	}
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *ManagedAgentsLedger) load() error {
	raw, err := os.ReadFile(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read managed agents ledger: %w", err)
	}
	var data managedAgentsLedgerData
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode managed agents ledger: %w", err)
	}
	if data.Days == nil {
		data.Days = map[string]managedAgentsLedgerDay{}
	}
	if data.Events == nil {
		data.Events = map[string]managedAgentsTokenEvent{}
	}
	if data.Reservations == nil {
		data.Reservations = map[string]managedAgentsReservation{}
	}
	for day, value := range data.Days {
		if value.Sessions == nil {
			value.Sessions = map[string]managedAgentsSessionUse{}
			data.Days[day] = value
		}
	}
	l.data = data
	return nil
}

// BudgetCents returns the configured maximum spend for one UTC day.
func (l *ManagedAgentsLedger) BudgetCents() int64 {
	if l == nil {
		return 0
	}
	return l.budgetCents
}

// DailySpendCents returns the cumulative list cost recorded for today's UTC
// day. It is safe to call from status and admission paths concurrently.
func (l *ManagedAgentsLedger) DailySpendCents() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.data.Days[l.dayKeyLocked()].SpendCents
}

// RemainingDailyCents returns the amount not already observed or reserved for
// another active hosted session today.
func (l *ManagedAgentsLedger) RemainingDailyCents() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.remainingLocked()
}

func (l *ManagedAgentsLedger) remainingLocked() int64 {
	dayKey := l.dayKeyLocked()
	spend := l.data.Days[dayKey].SpendCents
	reserved := int64(0)
	for _, reservation := range l.data.Reservations {
		if reservation.Day == dayKey {
			reserved += reservation.LimitCents
		}
	}
	remaining := l.budgetCents - spend - reserved
	if remaining < 0 {
		return 0
	}
	return remaining
}

// ReserveSession atomically reserves the current remaining budget for a
// session key. Reserving the full remainder serializes active hosted sessions,
// which keeps the aggregate spoke cap hard even if two starts race.
func (l *ManagedAgentsLedger) ReserveSession(sessionKey string) (int64, error) {
	if l == nil {
		return 0, errors.New("managed agents ledger is nil")
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return 0, errors.New("managed agents reservation key is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	dayKey := l.dayKeyLocked()
	if l.data.Reservations == nil {
		l.data.Reservations = map[string]managedAgentsReservation{}
	}
	previous := cloneManagedAgentsReservations(l.data.Reservations)
	for key, reservation := range l.data.Reservations {
		if reservation.Day != dayKey {
			delete(l.data.Reservations, key)
		}
	}
	if reservation, ok := l.data.Reservations[sessionKey]; ok {
		return reservation.LimitCents, nil
	}
	limit := l.remainingLocked()
	if limit < 1 {
		l.data.Reservations = previous
		return 0, ErrManagedAgentsBudgetExceeded
	}
	l.data.Reservations[sessionKey] = managedAgentsReservation{Day: dayKey, LimitCents: limit}
	if err := l.persistLocked(); err != nil {
		l.data.Reservations = previous
		return 0, err
	}
	return limit, nil
}

// MoveSessionReservation binds a pre-dispatch execution key to the vendor
// session ID returned by the start request.
func (l *ManagedAgentsLedger) MoveSessionReservation(from, to string) error {
	if l == nil {
		return errors.New("managed agents ledger is nil")
	}
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" {
		return errors.New("managed agents reservation keys are required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	reservation, ok := l.data.Reservations[from]
	if !ok || from == to {
		return nil
	}
	if _, exists := l.data.Reservations[to]; exists {
		return fmt.Errorf("managed agents reservation %q already exists", to)
	}
	previous := cloneManagedAgentsReservations(l.data.Reservations)
	delete(l.data.Reservations, from)
	l.data.Reservations[to] = reservation
	if err := l.persistLocked(); err != nil {
		l.data.Reservations = previous
		return err
	}
	return nil
}

// ReleaseSession removes an active reservation after the vendor session is
// terminal or has been confirmed absent.
func (l *ManagedAgentsLedger) ReleaseSession(sessionKey string) error {
	if l == nil {
		return errors.New("managed agents ledger is nil")
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return errors.New("managed agents reservation key is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.data.Reservations[sessionKey]; !ok {
		return nil
	}
	previous := cloneManagedAgentsReservations(l.data.Reservations)
	delete(l.data.Reservations, sessionKey)
	if err := l.persistLocked(); err != nil {
		l.data.Reservations = previous
		return err
	}
	return nil
}

func cloneManagedAgentsReservations(input map[string]managedAgentsReservation) map[string]managedAgentsReservation {
	output := make(map[string]managedAgentsReservation, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

// RecordModelRequest records one vendor model-request event. eventID is the
// vendor's durable event identity and is the deduplication key. The returned
// bool is true only when this event was newly recorded.
func (l *ManagedAgentsLedger) RecordModelRequest(sessionID, eventID, agent, model string, processedAt time.Time, input, output, cacheRead, cacheCreate int64) (bool, error) {
	if l == nil {
		return false, fmt.Errorf("managed agents ledger is nil")
	}
	sessionID = strings.TrimSpace(sessionID)
	eventID = strings.TrimSpace(eventID)
	if sessionID == "" || eventID == "" {
		return false, fmt.Errorf("managed agents session and event IDs are required")
	}
	if processedAt.IsZero() {
		processedAt = l.now()
	}
	event := managedAgentsTokenEvent{
		SessionID:   sessionID,
		EventID:     eventID,
		Agent:       strings.TrimSpace(agent),
		Model:       strings.TrimSpace(model),
		Input:       max(input, 0),
		Output:      max(output, 0),
		CacheRead:   max(cacheRead, 0),
		CacheCreate: max(cacheCreate, 0),
		Timestamp:   processedAt.UTC(),
	}
	key := eventID

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.data.Events[key]; exists {
		return false, nil
	}
	l.data.Events[key] = event
	if err := l.writeSessionLocked(sessionID); err != nil {
		delete(l.data.Events, key)
		return false, err
	}
	if err := l.persistLocked(); err != nil {
		delete(l.data.Events, key)
		return false, err
	}
	return true, nil
}

// RecordSessionUsage records the cumulative vendor list cost for one session.
// Repeated observations only add the increase, so polling cannot inflate the
// spoke ledger. The returned bool is true when today's hard cap is reached.
func (l *ManagedAgentsLedger) RecordSessionUsage(sessionID, currency string, listCostCents, activeSeconds, webSearches int64) (bool, error) {
	if l == nil {
		return false, fmt.Errorf("managed agents ledger is nil")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, fmt.Errorf("managed agents session ID is required")
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		currency = "USD"
	}
	if currency != "USD" {
		return false, fmt.Errorf("managed agents list cost currency %q is not USD", currency)
	}
	if listCostCents < 0 || activeSeconds < 0 || webSearches < 0 {
		return false, fmt.Errorf("managed agents usage values cannot be negative")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	dayKey := l.dayKeyLocked()
	oldDay, hadDay := l.data.Days[dayKey]
	oldSessions := oldDay.Sessions
	newDay := managedAgentsLedgerDay{SpendCents: oldDay.SpendCents, Sessions: make(map[string]managedAgentsSessionUse, len(oldSessions)+1)}
	for key, value := range oldSessions {
		newDay.Sessions[key] = value
	}
	previous := newDay.Sessions[sessionID]
	// The cumulative baseline spans days so a session that crosses UTC
	// midnight only adds its increase to the new day.
	baseline := previous.ListCostCents
	for _, day := range l.data.Days {
		if use, ok := day.Sessions[sessionID]; ok && use.ListCostCents > baseline {
			baseline = use.ListCostCents
		}
	}
	if listCostCents > baseline {
		newDay.SpendCents += listCostCents - baseline
	}
	if listCostCents > previous.ListCostCents {
		previous.ListCostCents = listCostCents
	}
	if activeSeconds > previous.ActiveSeconds {
		previous.ActiveSeconds = activeSeconds
	}
	if webSearches > previous.WebSearches {
		previous.WebSearches = webSearches
	}
	newDay.Sessions[sessionID] = previous
	l.data.Days[dayKey] = newDay
	if err := l.persistLocked(); err != nil {
		if hadDay {
			l.data.Days[dayKey] = oldDay
		} else {
			delete(l.data.Days, dayKey)
		}
		return false, err
	}
	return newDay.SpendCents >= l.budgetCents, nil
}

func (l *ManagedAgentsLedger) dayKeyLocked() string {
	return l.now().UTC().Format("2006-01-02")
}

func (l *ManagedAgentsLedger) persistLocked() error {
	raw, err := json.MarshalIndent(l.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode managed agents ledger: %w", err)
	}
	raw = append(raw, '\n')
	if err := atomicWrite(l.path, raw, 0o600); err != nil {
		return fmt.Errorf("write managed agents ledger: %w", err)
	}
	return nil
}

func (l *ManagedAgentsLedger) writeSessionLocked(sessionID string) error {
	events := make([]managedAgentsTokenEvent, 0)
	for _, event := range l.data.Events {
		if event.SessionID == sessionID {
			events = append(events, event)
		}
	}
	if len(events) == 0 {
		return nil
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].Timestamp.Equal(events[j].Timestamp) {
			return events[i].EventID < events[j].EventID
		}
		return events[i].Timestamp.Before(events[j].Timestamp)
	})

	var lines strings.Builder
	first := events[0]
	writeLine := func(value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		lines.Write(raw)
		lines.WriteByte('\n')
		return nil
	}
	if err := writeLine(SessionEntry{
		Type:      "message",
		Role:      "user",
		Agent:     first.Agent,
		Model:     first.Model,
		Backend:   BackendManagedAgents,
		Message:   "managed agent session " + sessionID,
		Timestamp: first.Timestamp.Format(time.RFC3339Nano),
	}); err != nil {
		return fmt.Errorf("encode managed agents session header: %w", err)
	}
	for _, event := range events {
		if err := writeLine(SessionEntry{
			Type:          "usage",
			Role:          "assistant",
			Agent:         event.Agent,
			Backend:       BackendManagedAgents,
			Model:         event.Model,
			InputTokens:   event.Input,
			OutputTokens:  event.Output,
			CacheRead:     event.CacheRead,
			CacheCreation: event.CacheCreate,
			Timestamp:     event.Timestamp.Format(time.RFC3339Nano),
		}); err != nil {
			return fmt.Errorf("encode managed agents session usage: %w", err)
		}
	}
	nameHash := sha256.Sum256([]byte(sessionID))
	path := filepath.Join(l.dir, "managed-agents-"+hex.EncodeToString(nameHash[:])[:32]+".jsonl")
	if err := atomicWrite(path, []byte(lines.String()), 0o600); err != nil {
		return fmt.Errorf("write managed agents session: %w", err)
	}
	return nil
}

func atomicWrite(path string, raw []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".managed-agents-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}
