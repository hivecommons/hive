package nousengine

import (
	"sync"

	"github.com/hivecommons/hive/pkg/config"
)

// FakeName is the engine name the test double registers under. It is NOT in
// config.ValidExperimentEngines, so an operator cannot select it; tests
// build it directly or through New(FakeName, …).
const FakeName = "fake"

func init() {
	Register(FakeName, func(s Settings) (Engine, error) { return NewFake(s.Dispatcher), nil })
}

// Fake is the engine contract's test double. Every face returns what the
// test put in it and records every Campaign it was asked about, so callers
// of the contract (dashboard, cmd/hive, future promotion code) can be tested
// without Nous or a filesystem.
type Fake struct {
	mu sync.Mutex

	dispatcher string
	// Calls records each method name and the campaign it was called with.
	Calls []FakeCall

	InvocationResult Invocation
	InvocationErr    error
	LayoutResult     Layout
	ProgressResult   []Round
	ProgressErr      error
	PrinciplesResult []Principle
	PrinciplesErr    error
	VerdictResult    Verdict
	// VerdictErr defaults to ErrNoVerdict so an unconfigured Fake behaves
	// like a campaign that has not finished.
	VerdictErr error
}

// FakeCall is one recorded Engine method call.
type FakeCall struct {
	Method   string
	Campaign Campaign
}

// NewFake returns a Fake reporting dispatcher (default inline) whose Verdict
// is ErrNoVerdict until a test sets one.
func NewFake(dispatcher string) *Fake {
	if dispatcher == "" {
		dispatcher = config.DefaultNousDispatcher
	}
	return &Fake{dispatcher: dispatcher, VerdictErr: ErrNoVerdict}
}

var _ Engine = (*Fake)(nil)

func (f *Fake) record(method string, c Campaign) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{Method: method, Campaign: c})
}

// Name implements Engine.
func (f *Fake) Name() string { return FakeName }

// Dispatcher implements Engine.
func (f *Fake) Dispatcher() string { return f.dispatcher }

// Invocation implements Engine.
func (f *Fake) Invocation(c Campaign) (Invocation, error) {
	f.record("Invocation", c)
	return f.InvocationResult, f.InvocationErr
}

// Layout implements Engine.
func (f *Fake) Layout(c Campaign) Layout {
	f.record("Layout", c)
	return f.LayoutResult
}

// Progress implements Engine.
func (f *Fake) Progress(c Campaign) ([]Round, error) {
	f.record("Progress", c)
	return f.ProgressResult, f.ProgressErr
}

// Principles implements Engine.
func (f *Fake) Principles(c Campaign) ([]Principle, error) {
	f.record("Principles", c)
	return f.PrinciplesResult, f.PrinciplesErr
}

// Verdict implements Engine.
func (f *Fake) Verdict(c Campaign) (Verdict, error) {
	f.record("Verdict", c)
	return f.VerdictResult, f.VerdictErr
}

// CallsTo returns the campaigns method was called with, in order.
func (f *Fake) CallsTo(method string) []Campaign {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Campaign
	for _, c := range f.Calls {
		if c.Method == method {
			out = append(out, c.Campaign)
		}
	}
	return out
}
