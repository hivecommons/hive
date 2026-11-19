package prosefilter

import "testing"

// TestDelegatesToCrux pins that the root-module entry point applies the
// shared crux filter rather than a copy of it.
func TestDelegatesToCrux(t *testing.T) {
	in := "a " + emDash + " b `c " + emDash + " d`"
	if got, want := Sanitize(in), "a - b `c "+emDash+" d`"; got != want {
		t.Fatalf("Sanitize = %q, want %q", got, want)
	}
	if got, want := SanitizeTitle("x "+enDash+" y"), "x - y"; got != want {
		t.Fatalf("SanitizeTitle = %q, want %q", got, want)
	}
	if !HasDash(emDash) || HasDash("-") {
		t.Fatal("HasDash wrong")
	}
	out, st := SanitizeJSON([]byte(`{"body":"a `+emDash+` b"}`), Keys{"body": Prose, "title": Title})
	if string(out) != `{"body":"a - b"}` || st.Fields != 1 {
		t.Fatalf("SanitizeJSON = %s (%+v)", out, st)
	}
}
