package api

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestPaths(t *testing.T) {
	if AgentPath("fixer") != "/v1/agents/fixer" || LogsPath("fixer") != "/v1/agents/fixer/logs" ||
		KickPath("fixer") != "/v1/agents/fixer/kick" || StopPath("fixer") != "/v1/agents/fixer/stop" {
		t.Fatalf("paths: %s %s %s %s", AgentPath("fixer"), LogsPath("fixer"), KickPath("fixer"), StopPath("fixer"))
	}
}

func TestParseAddress(t *testing.T) {
	cases := []struct {
		in   string
		want Address
	}{
		{"unix:///run/crux.sock", Address{Network: "unix", Path: "/run/crux.sock", BaseURL: unixBaseURL}},
		{"/run/crux.sock", Address{Network: "unix", Path: "/run/crux.sock", BaseURL: unixBaseURL}},
		{"./crux.sock", Address{Network: "unix", Path: "crux.sock", BaseURL: unixBaseURL}},
		{"../x/crux.sock", Address{Network: "unix", Path: "../x/crux.sock", BaseURL: unixBaseURL}},
		{" http://127.0.0.1:7777/ ", Address{Network: "tcp", Host: "127.0.0.1:7777", BaseURL: "http://127.0.0.1:7777"}},
		{"https://crux.example:7777", Address{Network: "tcp", Host: "crux.example:7777", BaseURL: "https://crux.example:7777"}},
		{"127.0.0.1:7777", Address{Network: "tcp", Host: "127.0.0.1:7777", BaseURL: "http://127.0.0.1:7777"}},
		{"[::1]:7777", Address{Network: "tcp", Host: "[::1]:7777", BaseURL: "http://[::1]:7777"}},
	}
	for _, c := range cases {
		got, err := ParseAddress(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q = %+v, want %+v", c.in, got, c.want)
		}
	}

	bad := []string{"", "   ", "unix://", "http://", "ftp://x:1", "localhost", "host:", "host:abc", "[::1:80", "http://%zz"}
	for _, in := range bad {
		if _, err := ParseAddress(in); err == nil {
			t.Errorf("%q should not parse", in)
		}
	}

	// Plain http carries the bearer token in the clear, so it is loopback-only;
	// the same hosts are fine over https.
	cleartext := []string{"http://crux.example:7777", "http://10.0.0.5:7777", "http://0.0.0.0:7777", "http://[::]:7777", "crux.example:7777", "10.0.0.5:7777", "0.0.0.0:7777", ":7777"}
	for _, in := range cleartext {
		if _, err := ParseAddress(in); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("%q must be refused as cleartext to a non-loopback host, got %v", in, err)
		}
	}
	for _, in := range []string{"https://10.0.0.5:7777", "https://0.0.0.0:7777", "http://localhost:7777", "http://[::ffff:127.0.0.1]:7777", "localhost:7777", "127.1.2.3:7777"} {
		if _, err := ParseAddress(in); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestLoopbackHost(t *testing.T) {
	yes := []string{"127.0.0.1:1", "127.255.0.9:80", "[::1]:1", "::1", "localhost", "LOCALHOST:7777", "[::ffff:127.0.0.1]:1", "[::1%lo]:1", "127.0.0.1"}
	for _, h := range yes {
		if !LoopbackHost(h) {
			t.Errorf("LoopbackHost(%q) = false, want true", h)
		}
	}
	no := []string{"", ":7777", "0.0.0.0:7777", "[::]:7777", "10.0.0.5:7777", "192.168.1.1", "crux.example:7777", "localhost.example:1", "[::ffff:10.0.0.5]:1", "127.0.0.1.nip.io:1"}
	for _, h := range no {
		if LoopbackHost(h) {
			t.Errorf("LoopbackHost(%q) = true, want false", h)
		}
	}
}

func TestWriteAndReadEvents(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteEvent(&buf, EventStarted, Event{Type: EventStarted, Agent: "fixer"}); err != nil {
		t.Fatal(err)
	}
	buf.WriteString(": keep-alive\n\n")
	buf.WriteString("id: 7\nevent: " + EventLog + "\ndata: {\"agent\":\"fixer\",\n")
	buf.WriteString("data:\"lines\":[\"a\"]}\n\n")
	buf.WriteString("event: trailing\ndata: {\"type\":\"trailing\"}") // no final blank line

	var got []string
	err := ReadEvents(&buf, func(typ string, data []byte) error {
		got = append(got, typ+"|"+string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		EventStarted + `|{"time":"0001-01-01T00:00:00Z","type":"agent.started","agent":"fixer"}`,
		EventLog + "|{\"agent\":\"fixer\",\n\"lines\":[\"a\"]}",
		`trailing|{"type":"trailing"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestReadEventsStopsOnCallbackError(t *testing.T) {
	stop := errors.New("stop")
	in := strings.NewReader("event: a\ndata: 1\n\nevent: b\ndata: 2\n\n")
	n := 0
	err := ReadEvents(in, func(string, []byte) error { n++; return stop })
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("err %v after %d records", err, n)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestReadEventsReportsReadErrors(t *testing.T) {
	if err := ReadEvents(failingReader{}, func(string, []byte) error { return nil }); err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteEventRejectsUnmarshalable(t *testing.T) {
	if err := WriteEvent(&bytes.Buffer{}, "x", make(chan int)); err == nil {
		t.Fatal("expected a marshal error")
	}
}
