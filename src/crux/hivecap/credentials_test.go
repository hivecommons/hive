package hivecap

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api/callback"
)

// credDirect is direct plus the agent_mint and bob families.
type credDirect struct {
	*direct
	mu       sync.Mutex
	enabled  bool
	mintErr  error
	mintReqs []callback.AgentMintRequest
	bob      callback.BobCredential
	bobErr   error
	bobCalls int
}

func (c *credDirect) MintAgentToken(_ context.Context, req callback.AgentMintRequest) (callback.AgentMintResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mintReqs = append(c.mintReqs, req)
	if c.mintErr != nil {
		return callback.AgentMintResponse{}, c.mintErr
	}
	if !c.enabled {
		return callback.AgentMintResponse{}, nil
	}
	return callback.AgentMintResponse{Token: "mint-" + req.Agent + "-" + req.Tier, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (c *credDirect) BobCredential(context.Context) (callback.BobCredential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bobCalls++
	return c.bob, c.bobErr
}

func unreachableClient(t *testing.T) *callback.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "hcap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	c, err := callback.NewClient("unix://"+filepath.Join(dir, "nobody.sock"), "")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The sidecar App token writer asks for exactly what the in-process writer
// asks AppAuth for — the agent's tier, installation-wide, no repos — and
// writes the token in place to the same cache path, with the same expiry
// whether hive is reached directly or over the socket.
func TestAppTokenWriterParity(t *testing.T) {
	exp := time.Now().Add(42 * time.Minute).Truncate(time.Second)
	run := func(t *testing.T, d *direct, caps callback.TokenMinter) (string, []byte) {
		t.Helper()
		cache := t.TempDir()
		w := NewAppTokenWriter(caps)
		w.CacheDir = cache
		w.injectOn = func() bool { return false }
		var logBuf bytes.Buffer
		w.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
		// Pre-created by the "entrypoint", as the real path expects.
		path := crux.AgentTokenCachePath(cache, "worker")
		if err := os.WriteFile(path, []byte("stale"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteAgentToken(context.Background(), "worker", "trusted", 1000); err != nil {
			t.Fatalf("WriteAgentToken: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.mints) != 1 || d.mints[0].Agent != "worker" || d.mints[0].Tier != "trusted" || d.mints[0].Repo != "" || len(d.mints[0].Repos) != 0 || d.mints[0].TTLSeconds != 0 {
			t.Errorf("mint request = %+v, want installation-wide trusted for worker", d.mints)
		}
		if strings.Contains(logBuf.String(), string(got)) {
			t.Errorf("token leaked into log:\n%s", logBuf.String())
		}
		if !strings.Contains(logBuf.String(), exp.UTC().Format("2006-01-02T15:04:05")) && !strings.Contains(logBuf.String(), exp.Format("2006-01-02T15:04:05")) {
			t.Errorf("expiry not reported: %s", logBuf.String())
		}
		if strings.Contains(logBuf.String(), "not pre-created") {
			t.Errorf("pre-created file misreported: %s", logBuf.String())
		}
		return filepath.Base(path), got
	}
	dd := &direct{expiry: exp}
	dp, dtok := run(t, dd, dd)
	sd := &direct{expiry: exp}
	sp, stok := run(t, sd, overSocket(t, sd))
	if dp != sp || !bytes.Equal(dtok, stok) || string(dtok) != "ghs_trusted_" {
		t.Errorf("direct (%s %q) vs socket (%s %q) differ", dp, dtok, sp, stok)
	}
}

func TestAppTokenWriterFailsClosed(t *testing.T) {
	cache := t.TempDir()
	path := crux.AgentTokenCachePath(cache, "w")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	check := func(t *testing.T, w *AppTokenWriter, want error) {
		t.Helper()
		w.CacheDir = cache
		w.injectOn = func() bool { return false }
		err := w.WriteAgentToken(ctx, "w", "t", 0)
		if err == nil || (want != nil && !errors.Is(err, want)) {
			t.Errorf("err = %v, want %v", err, want)
		}
		if got, _ := os.ReadFile(path); string(got) != "old" {
			t.Errorf("cache touched on failure: %q", got)
		}
	}
	t.Run("unreachable", func(t *testing.T) {
		check(t, NewAppTokenWriter(unreachableClient(t)), callback.ErrUnavailable)
	})
	t.Run("not configured", func(t *testing.T) {
		check(t, NewAppTokenWriter(overSocket(t, &direct{absent: map[string]bool{callback.CapabilityTokens: true}})), callback.ErrNotConfigured)
	})
	t.Run("expired", func(t *testing.T) {
		check(t, NewAppTokenWriter(&direct{expiry: time.Now().Add(-time.Second)}), nil)
	})
	t.Run("proxy injection", func(t *testing.T) {
		w := NewAppTokenWriter(&direct{})
		w.CacheDir = cache
		w.injectOn = func() bool { return true }
		if err := w.WriteAgentToken(ctx, "w", "t", 0); !errors.Is(err, ErrProxyInjectionUnsupported) {
			t.Errorf("err = %v", err)
		}
		if got, _ := os.ReadFile(path); string(got) != "old" {
			t.Errorf("cache touched: %q", got)
		}
	})
	t.Run("nil", func(t *testing.T) {
		var w *AppTokenWriter
		if err := w.WriteAgentToken(ctx, "w", "t", 0); err == nil {
			t.Error("nil writer must error")
		}
	})
	t.Run("default inject hook", func(t *testing.T) {
		if NewAppTokenWriter(&direct{}).injectOn == nil {
			t.Error("inject hook unset")
		}
	})
}

// A cache file the entrypoint did not pre-create is still written (0600)
// but the agent is told it cannot read it.
func TestAppTokenWriterCreatesAndWarns(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "nested")
	w := NewAppTokenWriter(&direct{})
	w.CacheDir = cache
	w.injectOn = func() bool { return false }
	var logBuf bytes.Buffer
	w.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
	if err := w.WriteAgentToken(context.Background(), "w", "t", 1000); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(crux.AgentTokenCachePath(cache, "w"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != agentTokenCachePerms {
		t.Errorf("mode = %v", st.Mode())
	}
	if !strings.Contains(logBuf.String(), "not pre-created") {
		t.Errorf("missing warning: %s", logBuf.String())
	}
	if w.dir() != cache || NewAppTokenWriter(&direct{}).dir() != crux.DefaultAgentTokenCacheDir {
		t.Error("dir default wrong")
	}
}

// The mint issuer is the in-process contract: "" with no error when mint is
// off or hive has no mint, the token otherwise, and an error (which the
// manager logs and swallows) only when hive cannot be reached.
func TestAgentMintIssuer(t *testing.T) {
	d := &credDirect{direct: &direct{}, enabled: true}
	for name, caps := range map[string]any{"direct": d, "socket": overSocket(t, d)} {
		t.Run(name, func(t *testing.T) {
			m := NewAgentMint(caps, 0)
			if !m.Enabled() {
				t.Fatal("must be enabled")
			}
			tok, err := m.MintAgentToken("a", "trusted")
			if err != nil || tok != "mint-a-trusted" {
				t.Errorf("mint = %q, %v", tok, err)
			}
		})
	}
	if len(d.mintReqs) != 2 || d.mintReqs[0] != d.mintReqs[1] || d.mintReqs[0].Agent != "a" {
		t.Errorf("requests differ across modes: %+v", d.mintReqs)
	}

	off := &credDirect{direct: &direct{}}
	if tok, err := NewAgentMint(off, 0).MintAgentToken("a", "t"); err != nil || tok != "" {
		t.Errorf("mint off = %q, %v", tok, err)
	}
	none := NewAgentMint(&direct{}, 0)
	if none.Enabled() {
		t.Error("plain capabilities cannot mint")
	}
	if tok, err := none.MintAgentToken("a", "t"); err != nil || tok != "" {
		t.Errorf("no family = %q, %v", tok, err)
	}
	absent := &credDirect{direct: &direct{absent: map[string]bool{callback.CapabilityAgentMint: true}}, enabled: true}
	if tok, err := NewAgentMint(overSocket(t, absent), 0).MintAgentToken("a", "t"); err != nil || tok != "" {
		t.Errorf("not configured over socket = %q, %v", tok, err)
	}
	if tok, err := NewAgentMint(unreachableClient(t), 200*time.Millisecond).MintAgentToken("a", "t"); tok != "" || !errors.Is(err, callback.ErrUnavailable) {
		t.Errorf("unreachable = %q, %v", tok, err)
	}
	failing := &credDirect{direct: &direct{}, enabled: true, mintErr: errors.New("boom")}
	if tok, err := NewAgentMint(failing, 0).MintAgentToken("a", "t"); tok != "" || err == nil {
		t.Errorf("failure = %q, %v", tok, err)
	}
}

func TestBobCredentialsCacheAndFailClosed(t *testing.T) {
	d := &credDirect{direct: &direct{}, bob: callback.BobCredential{APIKey: "bob-key", Source: "env:BOB_API_KEY"}}
	now := time.Unix(1_700_000_000, 0)
	b := NewBobCredentials(overSocket(t, d), 0)
	b.now = func() time.Time { return now }
	if b.Key() != "bob-key" || b.Source() != "env:BOB_API_KEY" {
		t.Fatalf("key/source = %q/%q", b.Key(), b.Source())
	}
	if d.bobCalls != 1 {
		t.Errorf("calls = %d, want one (cached)", d.bobCalls)
	}
	// Past TTL hive is asked again; a failure keeps the last answer for one
	// more TTL, then resolves empty.
	d.mu.Lock()
	d.bobErr = errors.New("boom")
	d.mu.Unlock()
	now = now.Add(DefaultBobCacheTTL + time.Second)
	if b.Key() != "bob-key" || d.bobCalls != 2 {
		t.Errorf("stale-but-served: key=%q calls=%d", b.Key(), d.bobCalls)
	}
	now = now.Add(DefaultBobCacheTTL)
	if b.Key() != "" || b.Source() != "" {
		t.Errorf("expired failure must resolve empty: %q/%q", b.Key(), b.Source())
	}
	d.mu.Lock()
	d.bobErr = nil
	d.mu.Unlock()
	if b.Key() != "bob-key" {
		t.Error("recovery not picked up")
	}

	if n := NewBobCredentials(&direct{}, 0); n.Key() != "" || n.Source() != "" {
		t.Error("capabilities without bob must resolve empty")
	}
	var nilB *BobCredentials
	if nilB.Key() != "" {
		t.Error("nil resolver must resolve empty")
	}
	u := NewBobCredentials(unreachableClient(t), 200*time.Millisecond)
	if u.Key() != "" || u.Source() != "" {
		t.Error("unreachable must resolve empty")
	}
	short := NewBobCredentials(d, 0)
	short.TTL = time.Hour
	if short.ttl() != time.Hour {
		t.Error("TTL override ignored")
	}
}

func TestSetCarriesCredentialWriters(t *testing.T) {
	s := New(&credDirect{direct: &direct{}, enabled: true}, 0)
	if s.AppTokens == nil || !s.Mint.Enabled() || s.Bob == nil {
		t.Errorf("Set missing credential writers: %+v", s)
	}
	if s2 := New(&direct{}, 0); s2.Mint.Enabled() {
		t.Error("plain capabilities must not claim mint")
	}
}
