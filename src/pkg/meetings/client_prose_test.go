package meetings

import (
	"context"
	"testing"
)

// Chat lines and spoken lines leave hive through Client.post, which applies
// the shared outbound dash rule (hivecommons/hive#19103).
func TestClientFiltersDashesInChatAndSpeech(t *testing.T) {
	f, srv := newFakeQuorumServer(t)
	c := NewClient(srv.URL, "tok", nil)
	ctx := context.Background()
	f.results["chat.post"] = `{"ok":true}`
	f.results["say"] = `{"ok":true}`
	f.results["say.prewarm"] = `{"ok":true}`
	if err := c.ChatPost(ctx, "q1", "noted — next item"); err != nil {
		t.Fatal(err)
	}
	if err := c.Say(ctx, "q1", "one — two"); err != nil {
		t.Fatal(err)
	}
	if err := c.SayPrewarm(ctx, "q1", []string{"three — four"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.args)
	if n < 3 {
		t.Fatalf("recorded %d tool calls, want at least 3", n)
	}
	if got := f.args[n-3]["text"]; got != "noted - next item" {
		t.Errorf("chat.post text = %v", got)
	}
	if got := f.args[n-2]["text"]; got != "one - two" {
		t.Errorf("say text = %v", got)
	}
	texts, _ := f.args[n-1]["texts"].([]any)
	if len(texts) != 1 || texts[0] != "three - four" {
		t.Errorf("say.prewarm texts = %v", f.args[n-1]["texts"])
	}
}
