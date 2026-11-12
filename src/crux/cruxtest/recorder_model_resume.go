package cruxtest

import "context"

// RestartAfterModelChange records the model-change restart (#17828).
func (r *Recorder) RestartAfterModelChange(_ context.Context, name, by string) error {
	r.record("RestartAfterModelChange", name, by)
	return r.errFor(name)
}
