package betty

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// publicWeb is a fake of the public sources: Wikipedia, Open-Meteo and the
// GitHub releases API on one test server. No test reaches the network.
type publicWeb struct {
	srv *httptest.Server
	mu  sync.Mutex
	// paths, agents and auth record every request.
	paths, agents, auth []string
	queries             []string
}

func newPublicWeb(t *testing.T) *publicWeb {
	t.Helper()
	p := &publicWeb{}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc("/w/api.php", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") != "opensearch" {
			http.NotFound(w, r)
			return
		}
		write(w, `["x",["Canberra (disambiguation)","Canberra"],["",""],["u1","u2"]]`)
	})
	mux.HandleFunc("/api/rest_v1/page/summary/", func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/api/rest_v1/page/summary/") {
		case "Canberra_(disambiguation)":
			write(w, `{"type":"disambiguation","title":"Canberra (disambiguation)","extract":"Canberra may refer to"}`)
		case "Canberra":
			write(w, `{"type":"standard","title":"Canberra","description":"Capital city of Australia","extract":"<b>Canberra</b> is the capital city of Australia.","content_urls":{"desktop":{"page":"https://en.wikipedia.org/wiki/Canberra"}}}`)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/v1/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") != "boston" {
			// Open-Meteo leaves "results" out when nothing matches.
			write(w, `{"generationtime_ms":0.1}`)
			return
		}
		write(w, `{"results":[{"name":"Boston","admin1":"Massachusetts","country":"United States","latitude":42.35843,"longitude":-71.05977}]}`)
	})
	mux.HandleFunc("/v1/forecast", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("latitude") != "42.3584" || q.Get("longitude") != "-71.0598" || q.Get("current") == "" || q.Get("daily") == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		write(w, `{"current":{"time":"2026-11-17T10:45","temperature_2m":8.4,"apparent_temperature":4.7,"weather_code":0,"wind_speed_10m":13.4,"relative_humidity_2m":65},
			"daily":{"temperature_2m_max":[11.8,9.0],"temperature_2m_min":[-0.7,1.5]}}`)
	})
	mux.HandleFunc("/repos/kubernetes/kubernetes/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"tag_name":"v1.37.2","name":"Kubernetes v1.37.2","published_at":"2026-11-12T08:00:00Z","html_url":"https://github.com/kubernetes/kubernetes/releases/tag/v1.37.2"}`)
	})
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.paths = append(p.paths, r.URL.Path)
		p.agents = append(p.agents, r.Header.Get("User-Agent"))
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		p.queries = append(p.queries, r.URL.RawQuery)
		p.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *publicWeb) requests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.paths)
}

func (p *publicWeb) direct(log *slog.Logger) DirectRetriever {
	u := p.srv.URL
	return DirectRetriever{Client: p.srv.Client(), Logger: log, WikipediaURL: u, GeocodeURL: u, ForecastURL: u, GitHubAPIURL: u,
		Guard: NewWebGuard("Betty", []string{"betty", "betty bee", "hive"}, []string{"acme/widgets"})}
}

func TestDirectWeather(t *testing.T) {
	p := newPublicWeb(t)
	var logs bytes.Buffer
	d := p.direct(slog.New(slog.NewTextHandler(&logs, nil)))
	if d.Name() != directName {
		t.Fatal(d.Name())
	}
	got := d.Retrieve(context.Background(), "Betty, what's the weather in Boston Massachusetts right now?", nil, retrieveLimit)
	if len(got) != 1 || got[0].Source != SourceOpenMeteo || got[0].Title != "Weather in Boston, Massachusetts, United States" {
		t.Fatalf("weather %+v", got)
	}
	for _, w := range []string{"Now (forecast-model estimate, not a station reading, can be off by several degrees): 8.4 C (47.1 F)", "feels like 4.7 C", "clear sky", "wind 13 km/h", "humidity 65%", "Today (forecast): high 11.8 C (53.2 F), low -0.7 C (30.7 F)", "Tomorrow (forecast): high 9.0 C", "According to Open-Meteo"} {
		if !strings.Contains(got[0].Text, w) {
			t.Errorf("weather text lacks %q: %s", w, got[0].Text)
		}
	}
	// "boston massachusetts" is split into the name and the state: one
	// geocoding call and one forecast.
	if p.requests() != 2 {
		t.Fatalf("requests %v", p.paths)
	}
	for i := range p.paths {
		if p.agents[i] != DefaultDirectUserAgent || p.auth[i] != "" {
			t.Fatalf("request %d: agent %q auth %q", i, p.agents[i], p.auth[i])
		}
	}
	// One log line: source, count, elapsed and the cleaned query, never
	// the utterance as said.
	line := logs.String()
	if strings.Count(line, "persona web retrieval") != 1 || !strings.Contains(line, "source=Open-Meteo") || !strings.Contains(line, "results=1") ||
		!strings.Contains(line, "elapsed_ms=") || strings.Contains(line, "Betty") || strings.Contains(line, "betty") {
		t.Fatalf("log %s", line)
	}
}

func TestDirectWeatherNeedsPlace(t *testing.T) {
	p := newPublicWeb(t)
	d := p.direct(nil)
	for _, q := range []string{"Betty, what's the weather where I live?", "what's the weather today", "is it raining"} {
		got := d.Retrieve(context.Background(), q, nil, retrieveLimit)
		if len(got) != 1 || got[0].Source != WeatherNeedsPlaceSource || !strings.Contains(got[0].Text, "ask which city") {
			t.Fatalf("%q: %+v", q, got)
		}
	}
	if p.requests() != 0 {
		t.Fatalf("a weather question with no place made a call: %v", p.paths)
	}
}

func TestDirectReleaseAndWikipedia(t *testing.T) {
	p := newPublicWeb(t)
	d := p.direct(nil)
	ctx := context.Background()
	got := d.Retrieve(ctx, "what is the latest stable release of Kubernetes?", nil, retrieveLimit)
	if len(got) != 1 || got[0].Source != SourceGitHubReleases || got[0].Title != "Latest release of kubernetes/kubernetes" ||
		!strings.Contains(got[0].Text, "v1.37.2 (Kubernetes v1.37.2), published 2026-11-12") || !strings.Contains(got[0].Text, "According to the GitHub releases API") {
		t.Fatalf("release %+v", got)
	}
	if p.requests() != 1 || p.auth[0] != "" || p.agents[0] != DefaultDirectUserAgent {
		t.Fatalf("release request %v auth %q", p.paths, p.auth)
	}
	// A project that is neither owner/repo nor well known is not asked.
	if d.Retrieve(ctx, "what is the latest release of some unknown thing", nil, retrieveLimit) != nil || p.requests() != 1 {
		t.Fatal("unknown project")
	}

	// The name and the framing are stripped before the topic is found.
	got = d.Retrieve(ctx, "Betty, can you tell me what the capital of Australia is?", nil, retrieveLimit)
	if len(got) != 1 || got[0].Source != SourceWikipedia || got[0].Title != "Canberra: Capital city of Australia" ||
		got[0].Text != "Canberra is the capital city of Australia. According to Wikipedia (https://en.wikipedia.org/wiki/Canberra)." {
		t.Fatalf("wikipedia %+v", got)
	}
	// opensearch, the disambiguation page (skipped), then the page.
	if n := p.requests(); n != 4 || !strings.Contains(p.queries[1], "search=capital+of+australia") {
		t.Fatalf("wikipedia requests %v %v", p.paths, p.queries)
	}
	if got = d.Retrieve(ctx, "tell me about WebRTC", nil, retrieveLimit); len(got) != 1 || !strings.Contains(p.queries[4], "search=WebRTC") {
		t.Fatalf("about %+v %v", got, p.queries)
	}
}

func TestDirectKeepsHiveQuestionsIn(t *testing.T) {
	p := newPublicWeb(t)
	d := p.direct(nil)
	for _, q := range []string{
		"how many open issues are there in total",
		"what do you know about hive",
		"what is hive",
		"what is the latest release of hive",
		"latest release of acme/widgets",
		"what is the weather in our office",
		"who is Betty",
		// Not a direct intent: left to the general search.
		"what's the difference between tcp and udp",
		"",
	} {
		if got := d.Retrieve(context.Background(), q, nil, retrieveLimit); got != nil {
			t.Errorf("%q: %+v", q, got)
		}
	}
	if d.Retrieve(context.Background(), "what is the weather in boston", nil, 0) != nil {
		t.Fatal("no room, no call")
	}
	if p.requests() != 0 {
		t.Fatalf("hive questions reached the public web: %v", p.paths)
	}
}

func TestDirectFailuresAndTimeout(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/search":
			_, _ = w.Write([]byte(`{"results":[{"name":"Boston","latitude":1,"longitude":2}]}`))
		case "/w/api.php":
			_, _ = w.Write([]byte(`{"not":"an array"}`))
		case "/repos/kubernetes/kubernetes/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":" "}`))
		default:
			http.Error(w, "limited", http.StatusTooManyRequests)
		}
	}))
	defer bad.Close()
	d := DirectRetriever{WikipediaURL: bad.URL, GeocodeURL: bad.URL, ForecastURL: bad.URL, GitHubAPIURL: bad.URL, UserAgent: "test-agent"}
	for _, q := range []string{"weather in boston", "what is kubernetes", "latest release of kubernetes"} {
		if got := d.Retrieve(context.Background(), q, nil, retrieveLimit); got != nil {
			t.Errorf("%q: %+v", q, got)
		}
	}
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	start := time.Now()
	sd := DirectRetriever{GeocodeURL: slow.URL, Timeout: 50 * time.Millisecond}
	if got := sd.Retrieve(context.Background(), "weather in boston", nil, retrieveLimit); got != nil {
		t.Fatal(got)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not honoured")
	}
}

func TestDirectIntents(t *testing.T) {
	places := []struct {
		q     string
		place string
		ok    bool
	}{
		{"what's the weather in boston right now", "boston", true},
		{"what's the weather where i live", "", true},
		{"weather today", "", true},
		{"what's the boston weather", "boston", true},
		{"is it raining in new york city today", "new york city", true},
		{"weather in my city", "", true},
		{"weather for tomorrow", "", true},
		{"how is the weather here", "", true},
		{"what's the weather like in the bay area", "bay area", true},
		{"what will the weather be like in paris tomorrow", "paris", true},
		{"what temperature does water boil at", "", false},
		{"what is kubernetes", "", false},
	}
	for _, c := range places {
		if place, ok := weatherPlace(c.q); place != c.place || ok != c.ok {
			t.Errorf("weatherPlace(%q) = %q, %v; want %q, %v", c.q, place, ok, c.place, c.ok)
		}
	}
	repos := []struct {
		q    string
		repo string
	}{
		{"what is the latest stable release of kubernetes", "kubernetes/kubernetes"},
		{"latest version of grafana/loki right now", "grafana/loki"},
		{"the newest release of argo cd", "argoproj/argo-cd"},
		{"kubernetes' latest release", "kubernetes/kubernetes"},
		{"latest release of cilium slash cilium", "cilium/cilium"},
		{"latest release of some unknown thing", ""},
		{"latest release of a/..", ""},
		{"what's new in kubernetes", ""},
	}
	for _, c := range repos {
		if repo, ok := releaseRepo(c.q); repo != c.repo || ok != (c.repo != "") {
			t.Errorf("releaseRepo(%q) = %q, %v; want %q", c.q, repo, ok, c.repo)
		}
	}
	topics := []struct {
		q     string
		about bool
		topic string
	}{
		{"what does webrtc stand for", false, "webrtc"},
		{"what is the capital of australia", false, "capital of australia"},
		{"what the capital of australia is", false, "capital of australia"},
		{"who was ada lovelace", false, "ada lovelace"},
		{"define idempotent", false, "idempotent"},
		{"explain what a service mesh is", false, "service mesh"},
		{"WebRTC", true, "WebRTC"},
		{"what's the difference between tcp and udp", false, ""},
		{"what is the time", false, ""},
		{"what are you doing", false, ""},
		{"what was the attendance at the last kubecon north america", false, ""},
		{"how do i install helm", false, ""},
	}
	for _, c := range topics {
		if topic, ok := definitionTopic(cleanedQuery{Query: c.q, About: c.about}); topic != c.topic || ok != (c.topic != "") {
			t.Errorf("definitionTopic(%q) = %q, %v; want %q", c.q, topic, ok, c.topic)
		}
	}
}

// searx is a fake SearXNG that records the queries it was sent.
type searx struct {
	srv  *httptest.Server
	mu   sync.Mutex
	got  []string
	body string
	code int
}

func newSearx(t *testing.T, body string) *searx {
	t.Helper()
	s := &searx{body: body, code: http.StatusOK}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.got = append(s.got, r.URL.Query().Get("q"))
		body, code := s.body, s.code
		s.mu.Unlock()
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *searx) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

func (s *searx) set(body string, code int) {
	s.mu.Lock()
	s.body, s.code = body, code
	s.mu.Unlock()
}

const searxOneHit = `{"results":[{"title":"One","url":"https://one.example/a","content":"first"}]}`

func TestWebRetrieverGuardAndCleanQuery(t *testing.T) {
	s := newSearx(t, searxOneHit)
	var logs bytes.Buffer
	w := WebRetriever{BaseURL: s.srv.URL, Client: s.srv.Client(), Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Guard: NewWebGuard("Betty", []string{"betty", "betty bee", "hive"}, []string{"acme/widgets"})}
	ctx := context.Background()
	// What went to Brave in the live test: none of it leaves the hive now.
	for _, q := range []string{
		"how many open issues are there in total",
		"what is pool request 18689 about",
		"tell me who hive's competitors are",
		"tell me all that you know about hive right now",
		"how many issues are in the high school",
		"what is the widgets roadmap",
	} {
		if got := w.Retrieve(ctx, q, nil, retrieveLimit); got != nil {
			t.Errorf("%q: %+v", q, got)
		}
	}
	if sent := s.sent(); len(sent) != 0 {
		t.Fatalf("hive questions reached the web: %v", sent)
	}
	// An explicit web search may go out, as the cleaned query.
	if got := w.Retrieve(ctx, "search the web for hive's competitors", nil, retrieveLimit); len(got) != 1 || got[0].Source != "web one.example" {
		t.Fatalf("explicit %+v", got)
	}
	// The persona's name and the framing are not sent.
	if got := w.Retrieve(ctx, "Betty, can you tell me what Kubernetes is, please?", nil, retrieveLimit); len(got) != 1 {
		t.Fatalf("cleaned %+v", got)
	}
	sent := s.sent()
	if len(sent) != 2 || sent[0] != "hive's competitors" || sent[1] != "what kubernetes is" {
		t.Fatalf("queries sent %q", sent)
	}
	line := logs.String()
	if strings.Count(line, "persona web retrieval") != 2 || !strings.Contains(line, "source=searxng") || !strings.Contains(line, "results=1") ||
		!strings.Contains(line, "elapsed_ms=") || strings.Contains(line, "Betty") || strings.Contains(line, "please") {
		t.Fatalf("log %s", line)
	}
}

func TestWebRetrieverInfoboxAndUnavailable(t *testing.T) {
	s := newSearx(t, `{"results":[{"title":"One","url":"https://one.example/a","content":"first"}],
		"infoboxes":[{"infobox":"Canberra","id":"https://en.wikipedia.org/wiki/Canberra","content":"Capital of <b>Australia</b>"},{"infobox":"Empty","id":"https://x.example","content":" "}]}`)
	w := WebRetriever{BaseURL: s.srv.URL, Client: s.srv.Client()}
	ctx := context.Background()
	got := w.Retrieve(ctx, "capital of australia", nil, retrieveLimit)
	if len(got) != 2 || got[0].Source != "web en.wikipedia.org" || got[0].Title != "Canberra" || !strings.HasPrefix(got[0].Text, "Capital of Australia (") || got[1].Title != "One" {
		t.Fatalf("infobox %+v", got)
	}
	one := func(note string) {
		t.Helper()
		got := w.Retrieve(ctx, "capital of australia", nil, retrieveLimit)
		if len(got) != 1 || got[0].Source != WebUnavailableSource || got[0].Text != note {
			t.Fatalf("want %q, got %+v", note, got)
		}
	}
	s.set(`{"results":[],"unresponsive_engines":[["brave","Suspended: too many requests"],["duckduckgo","CAPTCHA"]]}`, http.StatusOK)
	one(webNoteDead)
	s.set(`{"results":[]}`, http.StatusOK)
	one(webNoteEmpty)
	s.set(`limited`, http.StatusTooManyRequests)
	one(webNoteDown)
}

func TestWebRetrieverWithDirectSources(t *testing.T) {
	p := newPublicWeb(t)
	s := newSearx(t, searxOneHit)
	d := p.direct(nil)
	rs := PublicWebRetrievers(PublicWeb{SearxngURL: s.srv.URL, Client: s.srv.Client(), Guard: d.Guard})
	if len(rs) != 1 || PublicWebRetrievers(PublicWeb{}) != nil {
		t.Fatalf("web answers are opt-in: %d", len(rs))
	}
	w, ok := rs[0].(WebRetriever)
	if !ok || w.Direct == nil || w.Direct.Guard != nil {
		t.Fatalf("retriever %T", rs[0])
	}
	w.Direct = &d
	ctx := context.Background()
	// A weather answer is the direct source alone: search pages are cached
	// forecasts and are never offered beside it.
	got := w.Retrieve(ctx, "Betty, what's the weather in Boston right now?", nil, retrieveLimit)
	if len(got) != 1 || got[0].Source != SourceOpenMeteo {
		t.Fatalf("weather %+v", got)
	}
	// A direct answer stands when the search returns nothing.
	s.set(`{"results":[]}`, http.StatusOK)
	if got = w.Retrieve(ctx, "what is the latest stable release of kubernetes", nil, retrieveLimit); len(got) != 1 || got[0].Source != SourceGitHubReleases {
		t.Fatalf("release %+v", got)
	}
	// No place: she asks where, and nothing is searched.
	before, calls := len(s.sent()), p.requests()
	if got = w.Retrieve(ctx, "what's the weather where I live?", nil, retrieveLimit); len(got) != 1 || got[0].Source != WeatherNeedsPlaceSource {
		t.Fatalf("no place %+v", got)
	}
	if len(s.sent()) != before || p.requests() != calls {
		t.Fatal("a weather question with no place reached the web")
	}
	// Hive questions reach neither.
	if w.Retrieve(ctx, "what is hive", nil, retrieveLimit) != nil || len(s.sent()) != before || p.requests() != calls {
		t.Fatal("a hive question reached the web")
	}
}

// nwsWeb fakes the National Weather Service and Open-Meteo for a US town
// the geocoder only finds with its state.
func nwsWeb(t *testing.T, obsTime string, obsTemp string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	write := func(w http.ResponseWriter, body string) { _, _ = w.Write([]byte(body)) }
	mux.HandleFunc("/v1/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") != "carmel" {
			write(w, `{"generationtime_ms":0.1}`)
			return
		}
		write(w, `{"results":[
			{"name":"Carmel","admin1":"Indiana","country":"United States","country_code":"US","latitude":39.978,"longitude":-86.118},
			{"name":"Carmel","admin1":"New York","country":"United States","country_code":"US","latitude":41.4301,"longitude":-73.6801}]}`)
	})
	mux.HandleFunc("/v1/forecast", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("latitude") != "41.4301" {
			http.Error(w, "wrong place", http.StatusBadRequest)
			return
		}
		write(w, `{"current":{"time":"2026-11-18T07:15","temperature_2m":3.9,"apparent_temperature":1,"weather_code":0,"wind_speed_10m":5,"relative_humidity_2m":70},
			"daily":{"temperature_2m_max":[17,12],"temperature_2m_min":[3.9,5]}}`)
	})
	mux.HandleFunc("/points/", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"properties":{"observationStations":"`+srv.URL+`/gridpoints/OKX/39,76/stations"}}`)
	})
	mux.HandleFunc("/gridpoints/OKX/39,76/stations", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"features":[{"id":"`+srv.URL+`/stations/KPOU","properties":{"stationIdentifier":"KPOU"}}]}`)
	})
	mux.HandleFunc("/stations/KPOU/observations/latest", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"properties":{"timestamp":"`+obsTime+`","textDescription":"Mostly Cloudy","temperature":{"value":`+obsTemp+`}}}`)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestWeatherCityWithStateUsesStationReading(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 16, 0, 0, time.UTC)
	srv := nwsWeb(t, "2026-11-18T11:51:00+00:00", "9.4")
	d := DirectRetriever{Client: srv.Client(), GeocodeURL: srv.URL, ForecastURL: srv.URL, NWSURL: srv.URL, Now: func() time.Time { return now }}
	got := d.weather(context.Background(), "carmel new york")
	if len(got) != 1 || got[0].Source != SourceNWS || got[0].Title != "Weather in Carmel, New York, United States" {
		t.Fatalf("got %+v", got)
	}
	for _, w := range []string{"Now (measured at station KPOU at 11:51 UTC): 9.4 C (48.9 F), mostly cloudy", "Today (forecast): high 17.0 C", "According to National Weather Service"} {
		if !strings.Contains(got[0].Text, w) {
			t.Errorf("lacks %q: %s", w, got[0].Text)
		}
	}
	// The model's own "now" (3.9 C) is not presented as current.
	if strings.Contains(got[0].Text, "3.9 C (39.0 F), feels") {
		t.Errorf("model value stated as current: %s", got[0].Text)
	}
	// "carmel, ny" is the same place.
	if got = d.weather(context.Background(), "carmel, ny"); len(got) != 1 || !strings.Contains(got[0].Title, "New York") {
		t.Fatalf("postal code %+v", got)
	}
	// A state no match carries is a miss, not another state's weather.
	if got = d.weather(context.Background(), "carmel texas"); got != nil {
		t.Fatalf("wrong state answered: %+v", got)
	}
}

func TestWeatherStaleStationReadingFallsBackToLabelledModel(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 16, 0, 0, time.UTC)
	srv := nwsWeb(t, "2026-11-18T06:00:00+00:00", "9.4")
	d := DirectRetriever{Client: srv.Client(), GeocodeURL: srv.URL, ForecastURL: srv.URL, NWSURL: srv.URL, Now: func() time.Time { return now }}
	got := d.weather(context.Background(), "carmel new york")
	if len(got) != 1 || got[0].Source != SourceOpenMeteo || !strings.Contains(got[0].Text, "forecast-model estimate, not a station reading") ||
		strings.Contains(got[0].Text, "KPOU") {
		t.Fatalf("got %+v", got)
	}
}

func TestSplitRegion(t *testing.T) {
	for _, c := range []struct{ in, name, region string }{
		{"carmel new york", "carmel", "new york"},
		{"Carmel, NY", "carmel", "new york"},
		{"portland maine", "portland", "maine"},
		{"new york city", "new york city", ""},
		{"new york", "new york", ""},
		{"paris", "paris", ""},
		{"boston in", "boston in", ""},
	} {
		if n, r := splitRegion(c.in); n != c.name || r != c.region {
			t.Errorf("splitRegion(%q) = %q, %q; want %q, %q", c.in, n, r, c.name, c.region)
		}
	}
}

// Weather the direct sources cannot read is never answered from search
// pages: the live Meet test spoke cached forecasts of other days.
func TestWebRetrieverWeatherNeverFromSearchPages(t *testing.T) {
	s := newSearx(t, `{"results":[{"title":"Carmel forecast","url":"https://wx.example/a","content":"High 71"},{"title":"Carmel","url":"https://wx.example/b","content":"High 40"}]}`)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) }))
	defer bad.Close()
	d := DirectRetriever{GeocodeURL: bad.URL, ForecastURL: bad.URL, NWSURL: bad.URL}
	w := WebRetriever{BaseURL: s.srv.URL, Client: s.srv.Client(), Direct: &d}
	got := w.Retrieve(context.Background(), "what's the weather like in carmel new york", nil, retrieveLimit)
	if len(got) != 1 || got[0].Source != WeatherUnavailableSource {
		t.Fatalf("got %+v", got)
	}
	if !AnsweredLocally(got) || !strings.Contains(LocalAnswerNotes(got), "state no temperature") {
		t.Fatal("an unavailable note must stop research and forbid a figure")
	}
}

func TestResearchFailedTextIsShort(t *testing.T) {
	e := New(testSettings(), Deps{})
	long := "I couldn't get current conditions for Carmel, NY from any source I can reach. Search pages show stale forecasts (https://weather.com/a, https://wx.example/b) and the NWS site did not load, so check weather.gov directly."
	got := e.ResearchFailedText(long)
	if got != "I couldn't look that up: I couldn't get current conditions for Carmel, NY from any source I can reach." || strings.Contains(got, "http") {
		t.Fatalf("got %q", got)
	}
	if got = e.ResearchFailedText("   "); got != "I couldn't look that up: something went wrong." {
		t.Fatalf("empty %q", got)
	}
	if n := len([]rune(e.ResearchFailedText(strings.Repeat("word ", 100)))); n > 140 {
		t.Fatalf("not short: %d", n)
	}
}
