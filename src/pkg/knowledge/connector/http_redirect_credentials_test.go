package connector

import (
	"net/http"
	"net/url"
	"testing"
)

// The AllowPrivate redirect policy strips credentials off-host and refuses
// non-http(s) schemes, like the default CheckRedirectNoPrivate policy
// (hivecommons/hive#19222).
func TestCapRedirectsStripsCredentialsOffHost(t *testing.T) {
	mk := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		r := &http.Request{URL: u, Header: http.Header{}}
		r.Header.Set("Authorization", "Bearer secret")
		return r
	}
	via := []*http.Request{mk("http://10.0.0.5/api")}

	off := mk("http://10.0.0.6/api")
	if err := capRedirects(off, via); err != nil {
		t.Fatal(err)
	}
	if off.Header.Get("Authorization") != "" {
		t.Error("off-host hop kept Authorization")
	}
	same := mk("http://10.0.0.5/other")
	if err := capRedirects(same, via); err != nil {
		t.Fatal(err)
	}
	if same.Header.Get("Authorization") == "" {
		t.Error("same-host hop lost Authorization")
	}
	if err := capRedirects(mk("file:///etc/passwd"), via); err == nil {
		t.Error("file:// redirect followed")
	}
	if err := capRedirects(mk("http://10.0.0.5/x"), make([]*http.Request, 3)); err == nil {
		t.Error("fourth request in a chain followed")
	}
}
