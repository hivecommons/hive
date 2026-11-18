package reviewpreflight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// GetFunc fetches a URL and returns its body. It returns ErrNotFound for a
// 404 and a RateLimitError for 429.
type GetFunc func(ctx context.Context, url string) ([]byte, error)

// ErrNotFound means the registry has no such package or version.
var ErrNotFound = errors.New("not found")

// RateLimitError means the service asked the hive to slow down.
type RateLimitError struct{ Host string }

func (e RateLimitError) Error() string { return "rate limited by " + e.Host }

// parseTime reads the timestamp layouts registries use.
func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", sanitizeLine(truncate(s, 40)))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// goEscape applies the module proxy's case encoding: an upper-case letter
// becomes "!" and its lower-case form.
func goEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsUpper(r) {
			b.WriteByte('!')
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func goAge(ctx context.Context, get GetFunc, base string, d Dep) (time.Time, error) {
	body, err := get(ctx, base+"/"+goEscape(d.Name)+"/@v/"+url.PathEscape(d.Version)+".info")
	if err != nil {
		return time.Time{}, err
	}
	var v struct{ Time string }
	if err := json.Unmarshal(body, &v); err != nil {
		return time.Time{}, err
	}
	return parseTime(v.Time)
}

func npmAge(ctx context.Context, get GetFunc, base string, d Dep) (time.Time, error) {
	body, err := get(ctx, base+"/"+strings.ReplaceAll(d.Name, "/", "%2f"))
	if err != nil {
		return time.Time{}, err
	}
	var v struct {
		Time map[string]string `json:"time"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return time.Time{}, err
	}
	ts, ok := v.Time[d.Version]
	if !ok {
		return time.Time{}, ErrNotFound
	}
	return parseTime(ts)
}

func pypiAge(ctx context.Context, get GetFunc, base string, d Dep) (time.Time, error) {
	body, err := get(ctx, base+"/pypi/"+url.PathEscape(d.Name)+"/"+url.PathEscape(d.Version)+"/json")
	if err != nil {
		return time.Time{}, err
	}
	var v struct {
		URLs []struct {
			Uploaded string `json:"upload_time_iso_8601"`
		} `json:"urls"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return time.Time{}, err
	}
	var first time.Time
	for _, u := range v.URLs {
		t, err := parseTime(u.Uploaded)
		if err == nil && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}
	if first.IsZero() {
		return time.Time{}, ErrNotFound
	}
	return first, nil
}

func cratesAge(ctx context.Context, get GetFunc, base string, d Dep) (time.Time, error) {
	body, err := get(ctx, base+"/api/v1/crates/"+url.PathEscape(d.Name)+"/"+url.PathEscape(d.Version))
	if err != nil {
		return time.Time{}, err
	}
	var v struct {
		Version struct {
			Created string `json:"created_at"`
		} `json:"version"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return time.Time{}, err
	}
	return parseTime(v.Version.Created)
}

func mavenAge(ctx context.Context, get GetFunc, base string, d Dep) (time.Time, error) {
	g, a, ok := strings.Cut(d.Name, ":")
	if !ok {
		return time.Time{}, ErrNotFound
	}
	q := fmt.Sprintf(`g:"%s" AND a:"%s" AND v:"%s"`, g, a, d.Version)
	body, err := get(ctx, base+"/solrsearch/select?q="+url.QueryEscape(q)+"&core=gav&rows=1&wt=json")
	if err != nil {
		return time.Time{}, err
	}
	var v struct {
		Response struct {
			Docs []struct {
				Timestamp int64 `json:"timestamp"`
			} `json:"docs"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return time.Time{}, err
	}
	if len(v.Response.Docs) == 0 || v.Response.Docs[0].Timestamp <= 0 {
		return time.Time{}, ErrNotFound
	}
	return time.UnixMilli(v.Response.Docs[0].Timestamp).UTC(), nil
}

func rubygemsAge(ctx context.Context, get GetFunc, base string, d Dep) (time.Time, error) {
	body, err := get(ctx, base+"/api/v2/rubygems/"+url.PathEscape(d.Name)+"/versions/"+url.PathEscape(d.Version)+".json")
	if err != nil {
		return time.Time{}, err
	}
	var v struct {
		Created string `json:"created_at"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return time.Time{}, err
	}
	return parseTime(v.Created)
}

func packagistAge(ctx context.Context, get GetFunc, base string, d Dep) (time.Time, error) {
	body, err := get(ctx, base+"/p2/"+d.Name+".json")
	if err != nil {
		return time.Time{}, err
	}
	var v struct {
		Packages map[string][]struct {
			Version string `json:"version"`
			Time    string `json:"time"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return time.Time{}, err
	}
	for _, p := range v.Packages[d.Name] {
		if strings.TrimPrefix(p.Version, "v") == strings.TrimPrefix(d.Version, "v") {
			return parseTime(p.Time)
		}
	}
	return time.Time{}, ErrNotFound
}

func nugetAge(ctx context.Context, get GetFunc, base string, d Dep) (time.Time, error) {
	body, err := get(ctx, base+"/v3/registration5-semver1/"+url.PathEscape(strings.ToLower(d.Name))+"/"+url.PathEscape(strings.ToLower(d.Version))+".json")
	if err != nil {
		return time.Time{}, err
	}
	var v struct {
		Published string `json:"published"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return time.Time{}, err
	}
	return parseTime(v.Published)
}
