package transcribe

import (
	"context"
	"io"
	"sync"
)

// FakeName is the provider name the Fake adapter reports.
const FakeName = "fake"

// Fake is an in-memory Transcriber for tests. It reads the whole audio
// stream (so size limits and open errors behave as with a real upload),
// then returns Errs[i] for call i while any remain, else Result.
type Fake struct {
	Result Result
	Errs   []error
	// Ticks are reported through Audio.Progress after the audio is read,
	// in order; OnTick (when set) runs after each one, so a test can look at
	// the hive's live state mid-run.
	Ticks  []Progress
	OnTick func(Progress)

	mu    sync.Mutex
	calls int
	bytes int64
}

// Name implements Transcriber.
func (f *Fake) Name() string { return FakeName }

// Calls reports how many times Transcribe ran.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Bytes reports how many audio bytes the last call read.
func (f *Fake) Bytes() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bytes
}

// Transcribe implements Transcriber.
func (f *Fake) Transcribe(ctx context.Context, a Audio) (Result, error) {
	f.mu.Lock()
	i := f.calls
	f.calls++
	f.mu.Unlock()
	if a.Open != nil {
		rc, err := a.Open(ctx)
		if err != nil {
			return Result{}, err
		}
		n, err := io.Copy(io.Discard, rc)
		_ = rc.Close()
		f.mu.Lock()
		f.bytes = n
		f.mu.Unlock()
		if err != nil {
			return Result{}, err
		}
	}
	for _, p := range f.Ticks {
		a.Report(p)
		if f.OnTick != nil {
			f.OnTick(p)
		}
	}
	if i < len(f.Errs) && f.Errs[i] != nil {
		return Result{}, f.Errs[i]
	}
	res := f.Result
	if res.Provider == "" {
		res.Provider = FakeName
	}
	return res, nil
}
