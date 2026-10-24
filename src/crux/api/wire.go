package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
)

// Address is a parsed server address: where to dial and what URL to put in
// front of the routes. For a unix socket BaseURL is a fixed http://crux and
// the transport dials Path; for TCP BaseURL is the scheme and host and Path
// is empty.
type Address struct {
	// Network is "unix" or "tcp".
	Network string
	// Path is the socket path (unix only).
	Path string
	// Host is host:port (tcp only).
	Host string
	// BaseURL is the URL prefix requests are built on.
	BaseURL string
}

// unixBaseURL is the host clients use in URLs sent over a unix socket; the
// server never reads it.
const unixBaseURL = "http://crux"

// ParseAddress accepts the forms --server and CRUX_SERVER take:
//
//	unix:///run/user/1000/crux/crux.sock   a unix socket by URL
//	/run/user/1000/crux/crux.sock          a unix socket by absolute path
//	./crux.sock, ../x/crux.sock            a unix socket by relative path
//	http://127.0.0.1:7777                  a TCP listener by URL
//	https://crux.example:7777              a TLS listener by URL
//	127.0.0.1:7777, [::1]:7777             a TCP listener, http implied
//
// An empty string is an error so a caller cannot fall through to a default
// by accident.
func ParseAddress(s string) (Address, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Address{}, errors.New("empty server address")
	}
	switch {
	case strings.HasPrefix(s, "unix://"):
		p := strings.TrimPrefix(s, "unix://")
		if p == "" {
			return Address{}, fmt.Errorf("server address %q has no socket path", s)
		}
		return Address{Network: "unix", Path: p, BaseURL: unixBaseURL}, nil
	case strings.HasPrefix(s, "/"), strings.HasPrefix(s, "./"), strings.HasPrefix(s, "../"):
		return Address{Network: "unix", Path: filepath.Clean(s), BaseURL: unixBaseURL}, nil
	case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"):
		u, err := url.Parse(s)
		if err != nil {
			return Address{}, fmt.Errorf("server address %q: %w", s, err)
		}
		if u.Host == "" {
			return Address{}, fmt.Errorf("server address %q has no host", s)
		}
		return Address{Network: "tcp", Host: u.Host, BaseURL: u.Scheme + "://" + u.Host}, nil
	}
	if strings.Contains(s, "://") {
		return Address{}, fmt.Errorf("server address %q: unsupported scheme (use unix://, http:// or https://)", s)
	}
	// host:port with no scheme.
	if _, _, ok := splitHostPort(s); !ok {
		return Address{}, fmt.Errorf("server address %q is neither a socket path nor host:port", s)
	}
	return Address{Network: "tcp", Host: s, BaseURL: "http://" + s}, nil
}

// splitHostPort is net.SplitHostPort's shape without importing net, so this
// package stays free of anything a client might not want.
func splitHostPort(s string) (host, port string, ok bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 || i == len(s)-1 {
		return "", "", false
	}
	host, port = s[:i], s[i+1:]
	for _, r := range port {
		if r < '0' || r > '9' {
			return "", "", false
		}
	}
	if strings.HasPrefix(host, "[") != strings.HasSuffix(host, "]") {
		return "", "", false
	}
	return host, port, true
}

// WriteEvent writes one server-sent event: `event: <typ>` then the JSON of v
// on a `data:` line, then the blank line that ends the record. The caller
// flushes.
func WriteEvent(w io.Writer, typ string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data)
	return err
}

// ReadEvents decodes a text/event-stream body, calling fn with each record's
// type and raw data until r ends or fn returns an error, which is returned.
// Comment lines (`:` prefix, used as keep-alives) and unknown fields are
// skipped. A record with no data line is skipped.
func ReadEvents(r io.Reader, fn func(typ string, data []byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var typ string
	var data [][]byte
	flush := func() error {
		if len(data) == 0 {
			typ = ""
			return nil
		}
		err := fn(typ, bytes.Join(data, []byte("\n")))
		typ, data = "", nil
		return err
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			typ = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, []byte(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}
