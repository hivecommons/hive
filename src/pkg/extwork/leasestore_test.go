package extwork

import (
	"errors"
	"path/filepath"
	"testing"
)

// fakeAuthority vouches for exactly one lease tuple.
type fakeAuthority struct {
	identity, taskID, workKey, tier, stage string
	gen                                    uint64
	flushErr                               error
	flushes                                int
}

func (f *fakeAuthority) Verify(identity, taskID, workKey, tier, stage string, gen uint64) error {
	if identity != f.identity || taskID != f.taskID || workKey != f.workKey || tier != f.tier || stage != f.stage || gen != f.gen {
		return errors.New("no matching live lease")
	}
	return nil
}

func (f *fakeAuthority) Flush(identity, taskID, workKey, tier, stage string, gen uint64) error {
	if err := f.Verify(identity, taskID, workKey, tier, stage, gen); err != nil {
		return err
	}
	f.flushes++
	return f.flushErr
}

func TestLeaseStore(t *testing.T) {
	adm := testAdmission()
	auth := &fakeAuthority{identity: adm.Authority.Identity, taskID: adm.AssignmentID, workKey: adm.WorkKey, tier: adm.Authority.Tier, stage: adm.Stage, gen: adm.Generation}
	store := NewLeaseStore(auth, filepath.Join(t.TempDir(), "records"))

	bad := adm
	bad.WorkKey = ""
	if err := store.Persist(bad); !errors.Is(err, ErrInvalidAdmission) {
		t.Fatalf("invalid admission = %v", err)
	}
	for name, mutate := range map[string]func(*Admission){
		"identity": func(a *Admission) { a.Authority.Identity = "other" },
		"task":     func(a *Admission) { a.AssignmentID = "task-other" },
		"work key": func(a *Admission) { a.WorkKey = "other#1" },
		"tier":     func(a *Admission) { a.Authority.Tier = "T9" },
		"stage":    func(a *Admission) { a.Stage = "plan" },
		"gen":      func(a *Admission) { a.Generation++ },
	} {
		m := adm
		mutate(&m)
		if err := store.Persist(m); !errors.Is(err, ErrLeaseAuthority) {
			t.Errorf("%s mismatch: Persist = %v", name, err)
		}
	}
	if auth.flushes != 0 {
		t.Fatalf("mismatched admissions flushed the lease %d times", auth.flushes)
	}
	auth.flushErr = errors.New("disk full")
	if err := store.Persist(adm); !errors.Is(err, ErrLeaseAuthority) {
		t.Fatalf("flush failure = %v", err)
	}
	if _, ok, _ := store.Load(adm.AssignmentID); ok {
		t.Fatal("record written although the lease never reached disk")
	}
	auth.flushErr = nil
	if err := store.Persist(adm); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Load(adm.AssignmentID)
	if err != nil || !ok || got.ExecutionKey() != adm.ExecutionKey() {
		t.Fatalf("Load = %+v %v %v", got, ok, err)
	}
	if err := store.SaveReceipt(adm.AssignmentID, []byte("r")); err != nil {
		t.Fatal(err)
	}
	if raw, ok, err := store.LoadReceipt(adm.AssignmentID); err != nil || !ok || string(raw) != "r" {
		t.Fatalf("LoadReceipt = %q %v %v", raw, ok, err)
	}
	// The lease moves on (new generation): the record grants nothing.
	auth.gen++
	if _, ok, err := store.Load(adm.AssignmentID); ok || err != nil {
		t.Fatalf("Load after lease moved = %v %v", ok, err)
	}
	if _, ok, err := store.Load("../escape"); ok || !errors.Is(err, ErrBadAssignmentID) {
		t.Fatalf("Load unsafe id = %v %v", ok, err)
	}
}

type fakeRecorder struct {
	actor, action, agent string
	fields               map[string]any
	calls                int
}

func (r *fakeRecorder) Record(actor, action, agentName string, fields map[string]any) {
	r.calls++
	r.actor, r.action, r.agent, r.fields = actor, action, agentName, fields
}

func TestAuditProgressSink(t *testing.T) {
	rec := &fakeRecorder{}
	sink := NewAuditProgressSink(rec)
	sink.Record(ProgressEvent{Action: EventProgress, ExecutionKey: "k", AssignmentID: "task-1", State: StateWaiting, Fields: map[string]any{"stage": "instrument"}})
	if rec.calls != 1 || rec.actor != auditActorSystem || rec.action != EventProgress || rec.agent != "task-1" {
		t.Fatalf("recorded %+v", rec)
	}
	if rec.fields["execution_key"] != "k" || rec.fields["state"] != "waiting" || rec.fields["stage"] != "instrument" {
		t.Fatalf("fields = %v", rec.fields)
	}
	sink.Record(ProgressEvent{Action: EventOfferAccepted})
	if _, has := rec.fields["state"]; has {
		t.Fatal("empty state must not be recorded")
	}
	NewAuditProgressSink(nil).Record(ProgressEvent{Action: "x"})
	var nilSink *AuditProgressSink
	nilSink.Record(ProgressEvent{Action: "x"})
}
