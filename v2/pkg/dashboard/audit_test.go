package dashboard

import (
"encoding/json"
"net/http"
"net/http/httptest"
"testing"
"time"
)

func TestAuditLog_Log_DefaultsEmptyUserToSystem(t *testing.T) {
a := &AuditLog{
ring:       make([]AuditEntry, 0, auditRingCap),
lastAction: make(map[string]time.Time),
}
a.Log("", "test-action", "detail", "agent1")

entries := a.Recent(1)
if len(entries) != 1 {
t.Fatalf("expected 1 entry, got %d", len(entries))
}
if entries[0].User != "system" {
t.Errorf("expected user 'system', got %q", entries[0].User)
}
if entries[0].Action != "test-action" {
t.Errorf("expected action 'test-action', got %q", entries[0].Action)
}
}

func TestAuditLog_Log_RecordsAllFields(t *testing.T) {
a := &AuditLog{
ring:       make([]AuditEntry, 0, auditRingCap),
lastAction: make(map[string]time.Time),
}
a.Log("alice", "config-save", "key=value", "quality")

entries := a.Recent(1)
if len(entries) != 1 {
t.Fatalf("expected 1 entry, got %d", len(entries))
}
e := entries[0]
if e.User != "alice" {
t.Errorf("User = %q, want alice", e.User)
}
if e.Action != "config-save" {
t.Errorf("Action = %q, want config-save", e.Action)
}
if e.Detail != "key=value" {
t.Errorf("Detail = %q, want key=value", e.Detail)
}
if e.Agent != "quality" {
t.Errorf("Agent = %q, want quality", e.Agent)
}
if e.Timestamp == "" {
t.Error("Timestamp should not be empty")
}
}

func TestAuditLog_RingCapEnforced(t *testing.T) {
a := &AuditLog{
ring:       make([]AuditEntry, 0, auditRingCap),
lastAction: make(map[string]time.Time),
}

for i := 0; i < auditRingCap+50; i++ {
a.Log("user", "action", "", "")
}

entries := a.Recent(0)
if len(entries) > auditRingCap {
t.Errorf("ring exceeded cap: got %d, max %d", len(entries), auditRingCap)
}
}

func TestAuditLog_Recent_ReturnsNewestFirst(t *testing.T) {
a := &AuditLog{
ring:       make([]AuditEntry, 0, auditRingCap),
lastAction: make(map[string]time.Time),
}
a.Log("alice", "first", "", "")
time.Sleep(time.Millisecond)
a.Log("bob", "second", "", "")
time.Sleep(time.Millisecond)
a.Log("carol", "third", "", "")

entries := a.Recent(3)
if len(entries) != 3 {
t.Fatalf("expected 3 entries, got %d", len(entries))
}
if entries[0].Action != "third" {
t.Errorf("newest entry should be first; got action=%q", entries[0].Action)
}
if entries[2].Action != "first" {
t.Errorf("oldest entry should be last; got action=%q", entries[2].Action)
}
}

func TestAuditLog_Recent_LimitsOutput(t *testing.T) {
a := &AuditLog{
ring:       make([]AuditEntry, 0, auditRingCap),
lastAction: make(map[string]time.Time),
}
for i := 0; i < 10; i++ {
a.Log("user", "action", "", "")
}

entries := a.Recent(3)
if len(entries) != 3 {
t.Errorf("expected 3 entries, got %d", len(entries))
}
}

func TestAuditLog_NoteUserAction_SkipsPseudoUsers(t *testing.T) {
a := &AuditLog{
ring:       make([]AuditEntry, 0, auditRingCap),
lastAction: make(map[string]time.Time),
}

for pseudo := range auditPseudoUsers {
a.Log(pseudo, "action", "", "")
}

actions := a.LastUserActions()
if len(actions) != 0 {
t.Errorf("pseudo-users should not appear in LastUserActions; got %v", actions)
}
}

func TestAuditLog_NoteUserAction_TracksRealUsers(t *testing.T) {
a := &AuditLog{
ring:       make([]AuditEntry, 0, auditRingCap),
lastAction: make(map[string]time.Time),
}
a.Log("alice", "login", "", "")

actions := a.LastUserActions()
if _, ok := actions["alice"]; !ok {
t.Error("expected alice in LastUserActions")
}
if _, err := time.Parse(time.RFC3339, actions["alice"]); err != nil {
t.Errorf("timestamp should be RFC3339: %v", err)
}
}

func TestAuditLog_NoteUserAction_NeverMovesBackward(t *testing.T) {
a := &AuditLog{
ring:       make([]AuditEntry, 0, auditRingCap),
lastAction: make(map[string]time.Time),
}

a.mu.Lock()
a.noteUserAction("alice", "2024-01-01T00:00:00Z")
a.noteUserAction("alice", "2024-06-15T12:00:00Z")
a.noteUserAction("alice", "2024-03-01T00:00:00Z")
a.mu.Unlock()

actions := a.LastUserActions()
if actions["alice"] != "2024-06-15T12:00:00Z" {
t.Errorf("expected latest timestamp, got %q", actions["alice"])
}
}

func TestAuditLog_NoteUserAction_BoundsMapSize(t *testing.T) {
a := &AuditLog{
ring:       make([]AuditEntry, 0, auditRingCap),
lastAction: make(map[string]time.Time),
}

a.mu.Lock()
for i := 0; i < auditMaxTrackedActionUsers; i++ {
a.lastAction[time.Now().Format("user-150405.000000000")] = time.Now()
}
a.noteUserAction("overflow-user", time.Now().UTC().Format(time.RFC3339))
a.mu.Unlock()

if _, ok := a.lastAction["overflow-user"]; ok {
t.Error("map should not grow beyond auditMaxTrackedActionUsers for new users")
}
}

func TestHandleAuditLog_ForbidsInsufficientRole(t *testing.T) {
s := newTestServer()
mux := http.NewServeMux()
mux.HandleFunc("GET /api/audit", s.handleAuditLog)

req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
req.Header.Set("X-Hive-Role", "readonly")
rec := httptest.NewRecorder()
mux.ServeHTTP(rec, req)

if rec.Code != http.StatusForbidden {
t.Errorf("expected 403 for readonly role, got %d", rec.Code)
}
}

func TestHandleAuditLog_AllowsReadWriteRole(t *testing.T) {
s := newTestServer()
s.audit.Log("alice", "test", "", "")

mux := http.NewServeMux()
mux.HandleFunc("GET /api/audit", s.handleAuditLog)

req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
req.Header.Set("X-Hive-Role", "readwrite")
rec := httptest.NewRecorder()
mux.ServeHTTP(rec, req)

if rec.Code != http.StatusOK {
t.Fatalf("expected 200, got %d", rec.Code)
}

var body map[string]json.RawMessage
if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
t.Fatalf("invalid JSON response: %v", err)
}
if _, ok := body["entries"]; !ok {
t.Error("response should contain 'entries' key")
}
}

func TestHandleAuditLog_DefaultsToOwnerRole(t *testing.T) {
s := newTestServer()
mux := http.NewServeMux()
mux.HandleFunc("GET /api/audit", s.handleAuditLog)

req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
rec := httptest.NewRecorder()
mux.ServeHTTP(rec, req)

if rec.Code != http.StatusOK {
t.Errorf("expected 200 with default owner role, got %d", rec.Code)
}
}

func TestAuditDetail_FormatsKeyValuePairs(t *testing.T) {
tests := []struct {
input []string
want  string
}{
{nil, ""},
{[]string{"k1", "v1"}, "k1=v1"},
{[]string{"k1", "v1", "k2", "v2"}, "k1=v1, k2=v2"},
{[]string{"odd"}, ""},
}
for _, tt := range tests {
got := auditDetail(tt.input...)
if got != tt.want {
t.Errorf("auditDetail(%v) = %q, want %q", tt.input, got, tt.want)
}
}
}
