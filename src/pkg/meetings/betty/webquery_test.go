package betty

import "testing"

func TestCleanWebQuery(t *testing.T) {
	cases := []struct {
		in       string
		want     string
		explicit bool
	}{
		{"can you tell me what the capital of Australia is?", "what the capital of Australia is", false},
		{"Please, could you tell us what WebRTC stands for?", "what WebRTC stands for", false},
		{"do you know who wrote the Go memory model", "who wrote the Go memory model", false},
		{"what's the weather in boston right now?", "what's the weather in boston right now", false},
		{"search the web for hive's competitors", "hive's competitors", true},
		{"can you search the web for the best open source meeting bots, please?", "the best open source meeting bots", true},
		{"please do a web search for kubernetes gateway api", "kubernetes gateway api", true},
		{"look up the latest stable release of kubernetes online", "the latest stable release of kubernetes", true},
		{"look up the latest stable release of kubernetes", "the latest stable release of kubernetes", false},
		{"google rust borrow checker", "rust borrow checker", true},
		{"tell me about WebRTC.", "WebRTC", false},
		{"I was wondering if TCP is reliable, thanks", "TCP is reliable", false},
		{"um, so, what is a service mesh?!", "what is a service mesh", false},
		{"what is redis available online", "what is redis available online", false},
		{"can you search the web", "", true},
		{"  ?? ", "", false},
	}
	for _, c := range cases {
		got, explicit := CleanWebQuery(c.in)
		if got != c.want || explicit != c.explicit {
			t.Errorf("CleanWebQuery(%q) = %q, %v; want %q, %v", c.in, got, explicit, c.want, c.explicit)
		}
	}
	if c := cleanQuery("tell me about WebRTC"); !c.About || c.Query != "WebRTC" {
		t.Fatalf("about %+v", c)
	}
}

func TestWebGuard(t *testing.T) {
	g := NewWebGuard("Betty", []string{"betty", "betty bee", "hive"}, []string{"acme", "acme/widgets", "acme/console-docs", "x"})
	cases := []struct {
		in    string
		query string
		ok    bool
	}{
		// The persona's name and the framing never reach the web.
		{"Betty, can you tell me what the capital of Australia is?", "what the capital of australia is", true},
		{"what does webrtc stand for", "what does webrtc stand for", true},
		{"hive, what's the weather in boston right now?", "what's the weather in boston right now", true},
		// The hive's own state and product stay inside.
		{"how many open issues are there in total", "", false},
		{"how many issues are in the high school", "", false},
		{"what is pool request 18689 about", "", false},
		{"what is the board pressure right now", "", false},
		{"which agents are running right now", "", false},
		{"tell me all that you know about hive right now", "", false},
		{"tell me who hive's competitors are", "", false},
		{"what do you know about Hives competitors", "", false},
		{"tell me which voice memo providers hive supports", "", false},
		{"which meeting platforms do you support", "", false},
		{"what is this hive doing", "", false},
		{"when is our repo getting a release", "", false},
		{"what did we decide about the rollout", "", false},
		{"who is betty bee", "", false},
		{"what is the widgets roadmap", "", false},
		{"any news on acme", "", false},
		{"is console docs up to date", "", false},
		{"what was said in this meeting", "", false},
		{"", "", false},
		// Asked for by name: the web search goes out, cleaned.
		{"search the web for hive's competitors", "hive's competitors", true},
		{"Betty, search the web for Hives competitors.", "hive's competitors", true},
		{"can you look up acme reviews online?", "acme reviews", true},
	}
	for _, c := range cases {
		q, ok := g.Query(c.in)
		if ok != c.ok || (c.ok && q != c.query) {
			t.Errorf("Query(%q) = %q, %v; want %q, %v", c.in, q, ok, c.query, c.ok)
		}
	}
	// A nil guard still keeps hive questions in; it only lacks the names.
	var none *WebGuard
	if _, ok := none.Query("how many open issues are there"); ok {
		t.Fatal("nil guard let a hive status question out")
	}
	if q, ok := none.Query("What is Kubernetes?"); !ok || q != "What is Kubernetes" {
		t.Fatalf("nil guard %q %v", q, ok)
	}
	if _, blocked := g.check("what is the widgets roadmap"); blocked != webBlockedName {
		t.Fatalf("blocked %q", blocked)
	}
}
