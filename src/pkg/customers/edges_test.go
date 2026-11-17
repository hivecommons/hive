package customers

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openIndex(t *testing.T) *Index {
	t.Helper()
	x, err := OpenIndex(filepath.Join(t.TempDir(), "graph"))
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	t.Cleanup(func() { _ = x.Close() })
	return x
}

var edgeEpoch = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func edge(person, pred, ref, via string, hours int) Edge {
	return Edge{Person: person, Predicate: pred, Artefact: ref, Via: via, Source: "test", At: edgeEpoch.Add(time.Duration(hours) * time.Hour)}
}

func TestIndexFileIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "graph")
	x, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	info, err := os.Stat(filepath.Join(dir, IndexFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("customers.db mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestPutReadsNewestFirstAndDedupes(t *testing.T) {
	x := openIndex(t)
	n, err := x.Put([]Edge{
		edge("p_a", PredAuthored, IssueRef("o/r", 1), "github:ada", 1),
		edge("p_a", PredReviewed, PRRef("o/r", 2), "github:ada", 3),
		edge("p_a", PredAttended, MeetingRef("m1"), "email:ada@example.com", 2),
		edge("p_b", PredAuthored, IssueRef("o/r", 9), "github:bo", 5),
		// Unstorable edges are skipped, not written.
		{Person: "", Predicate: PredAuthored, Artefact: IssueRef("o/r", 3), At: edgeEpoch},
		{Person: "p_a", Predicate: "", Artefact: IssueRef("o/r", 3), At: edgeEpoch},
		{Person: "p_a", Predicate: PredAuthored, Artefact: "no-kind", At: edgeEpoch},
		{Person: "p_a", Predicate: PredAuthored, Artefact: "gh:bad\x00ref", At: edgeEpoch},
		{Person: "p_a", Predicate: PredAuthored, Artefact: IssueRef("o/r", 3)},
	})
	if err != nil || n != 4 {
		t.Fatalf("Put = %d, %v; want 4", n, err)
	}
	edges, next, err := x.ByPerson("p_a", "", "", 0)
	if err != nil || next != "" || len(edges) != 3 {
		t.Fatalf("ByPerson = %d edges, next %q, %v", len(edges), next, err)
	}
	if edges[0].Predicate != PredReviewed || edges[1].Predicate != PredAttended || edges[2].Predicate != PredAuthored {
		t.Fatalf("order = %s, %s, %s; want newest first", edges[0].Predicate, edges[1].Predicate, edges[2].Predicate)
	}
	only, _, err := x.ByPerson("p_a", PredAttended, "", 0)
	if err != nil || len(only) != 1 || only[0].Artefact != MeetingRef("m1") {
		t.Fatalf("predicate filter = %+v, %v", only, err)
	}

	// The same (person, predicate, artefact) seen again replaces the row.
	if _, err := x.Put([]Edge{edge("p_a", PredAuthored, IssueRef("o/r", 1), "github:ada", 10)}); err != nil {
		t.Fatal(err)
	}
	edges, _, _ = x.ByPerson("p_a", "", "", 0)
	if len(edges) != 3 || edges[0].Predicate != PredAuthored || x.Len() != 4 {
		t.Fatalf("after re-sighting: %d edges for p_a, %d total, first %s", len(edges), x.Len(), edges[0].Predicate)
	}
	counts, err := x.Counts("p_a")
	if err != nil || counts[PredAuthored] != 1 || counts[PredReviewed] != 1 || len(counts) != 3 {
		t.Fatalf("Counts = %v, %v", counts, err)
	}
	// A person id that is a prefix of another must not read its edges.
	if got, _, _ := x.ByPerson("p_", "", "", 0); len(got) != 0 {
		t.Fatalf("prefix id read %d edges", len(got))
	}
}

func TestByPersonPages(t *testing.T) {
	x := openIndex(t)
	var all []Edge
	for i := 0; i < 7; i++ {
		all = append(all, edge("p_a", PredAuthored, IssueRef("o/r", i+1), "github:ada", i))
	}
	if _, err := x.Put(all); err != nil {
		t.Fatal(err)
	}
	var seen []string
	after := ""
	for page := 0; page < 5; page++ {
		edges, next, err := x.ByPerson("p_a", "", after, 3)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, e := range edges {
			seen = append(seen, e.Artefact)
		}
		if next == "" {
			break
		}
		after = next
	}
	if len(seen) != 7 || seen[0] != IssueRef("o/r", 7) || seen[6] != IssueRef("o/r", 1) {
		t.Fatalf("paged read = %v", seen)
	}
	if edges, next, _ := x.ByPerson("p_a", "", "", 7); len(edges) != 7 || next != "" {
		t.Fatalf("an exact page must not promise another: %d, %q", len(edges), next)
	}
	if edges, _, _ := x.ByPerson("p_a", "", "", MaxEdgePage+50); len(edges) != 7 {
		t.Fatalf("oversize limit = %d", len(edges))
	}
	if _, _, err := x.ByPerson("p_a", "", "zz", 3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("garbage cursor = %v", err)
	}
	// A cursor from one person cannot be replayed against another.
	_, next, _ := x.ByPerson("p_a", "", "", 3)
	if _, _, err := x.ByPerson("p_b", "", next, 3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign cursor = %v", err)
	}
}

func TestByArtefactWalksBack(t *testing.T) {
	x := openIndex(t)
	ref := MeetingRef("m1")
	if _, err := x.Put([]Edge{
		edge("p_a", PredAttended, ref, "email:a@example.com", 1),
		edge("p_b", PredOrganised, ref, "email:b@example.com", 1),
		edge("p_b", PredAttended, MeetingRef("m10"), "email:b@example.com", 1),
	}); err != nil {
		t.Fatal(err)
	}
	edges, err := x.ByArtefact(ref)
	if err != nil || len(edges) != 2 {
		t.Fatalf("ByArtefact = %+v, %v", edges, err)
	}
	people := map[string]string{}
	for _, e := range edges {
		people[e.Person] = e.Predicate
	}
	if people["p_a"] != PredAttended || people["p_b"] != PredOrganised {
		t.Fatalf("people = %v", people)
	}
}

func TestDeletePersonRemovesEveryEdge(t *testing.T) {
	x := openIndex(t)
	ref := IssueRef("o/r", 1)
	if _, err := x.Put([]Edge{
		edge("p_a", PredAuthored, ref, "github:ada", 1),
		edge("p_a", PredAttended, MeetingRef("m1"), "email:a@example.com", 2),
		edge("p_b", PredCommentedOn, ref, "github:bo", 3),
	}); err != nil {
		t.Fatal(err)
	}
	n, err := x.DeletePerson("p_a")
	if err != nil || n != 2 {
		t.Fatalf("DeletePerson = %d, %v", n, err)
	}
	if edges, _, _ := x.ByPerson("p_a", "", "", 0); len(edges) != 0 {
		t.Fatalf("forgotten person still has edges: %+v", edges)
	}
	back, _ := x.ByArtefact(ref)
	if len(back) != 1 || back[0].Person != "p_b" {
		t.Fatalf("the reverse walk still reaches the forgotten person: %+v", back)
	}
	if x.Len() != 1 {
		t.Fatalf("Len = %d", x.Len())
	}
}

func TestMoveByViaMakesSplitExact(t *testing.T) {
	x := openIndex(t)
	if _, err := x.Put([]Edge{
		edge("p_a", PredAuthored, IssueRef("o/r", 1), "github:ada", 1),
		edge("p_a", PredSent, InboxRef("email", "m1"), "email:shared@example.com", 2),
		edge("p_a", PredInvited, EventRef("e1"), "email:shared@example.com", 3),
	}); err != nil {
		t.Fatal(err)
	}
	n, err := x.Move("p_a", "p_new", ViaAny([]string{"email:shared@example.com"}))
	if err != nil || n != 2 {
		t.Fatalf("Move = %d, %v", n, err)
	}
	stay, _, _ := x.ByPerson("p_a", "", "", 0)
	moved, _, _ := x.ByPerson("p_new", "", "", 0)
	if len(stay) != 1 || stay[0].Via != "github:ada" || len(moved) != 2 {
		t.Fatalf("after split: stay=%+v moved=%+v", stay, moved)
	}
	for _, e := range moved {
		if e.Person != "p_new" {
			t.Fatalf("moved edge kept the old person: %+v", e)
		}
	}
	if back, _ := x.ByArtefact(EventRef("e1")); len(back) != 1 || back[0].Person != "p_new" {
		t.Fatalf("reverse index after move = %+v", back)
	}
	// Merge moves everything.
	if n, _ := x.Move("p_new", "p_a", func(Edge) bool { return true }); n != 2 {
		t.Fatalf("merge move = %d", n)
	}
	if _, err := x.Move("p_a", "p_a", func(Edge) bool { return true }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("move onto itself = %v", err)
	}
	if _, err := x.Move("p_a", "", func(Edge) bool { return true }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("move to nobody = %v", err)
	}
}

// TestPruneRemovesEdgesOfPurgedArtefacts is the retention contract: when
// an artefact is gone, no profile can still point at it.
func TestPruneRemovesEdgesOfPurgedArtefacts(t *testing.T) {
	x := openIndex(t)
	purged := MeetingRef("m1")
	if _, err := x.Put([]Edge{
		edge("p_a", PredAttended, purged, "email:a@example.com", 1),
		edge("p_b", PredSpokeIn, purged, "email:b@example.com", 1),
		edge("p_a", PredAttended, MeetingRef("m2"), "email:a@example.com", 2),
		edge("p_a", PredAuthored, IssueRef("o/r", 1), "github:ada", 3),
	}); err != nil {
		t.Fatal(err)
	}
	asked := map[string]int{}
	exists := func(ref string) bool {
		asked[ref]++
		return ref != purged
	}
	n, err := x.Prune(exists)
	if err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v; want 2", n, err)
	}
	if asked[purged] != 1 {
		t.Fatalf("the resolver was asked %d times about one artefact", asked[purged])
	}
	if back, _ := x.ByArtefact(purged); len(back) != 0 {
		t.Fatalf("edges to a purged meeting survive: %+v", back)
	}
	left, _, _ := x.ByPerson("p_a", "", "", 0)
	if len(left) != 2 {
		t.Fatalf("p_a keeps %d edges, want 2", len(left))
	}
	for _, e := range left {
		if e.Artefact == purged {
			t.Fatal("the profile still points at the purged meeting")
		}
	}
	if n, _ := x.Prune(exists); n != 0 {
		t.Fatalf("second prune removed %d", n)
	}
}

func TestIndexReopens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "graph")
	x, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.Put([]Edge{edge("p_a", PredAuthored, IssueRef("o/r", 1), "github:ada", 1)}); err != nil {
		t.Fatal(err)
	}
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.Len() != 1 {
		t.Fatalf("Len after reopen = %d", again.Len())
	}
}
