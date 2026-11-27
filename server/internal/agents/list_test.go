package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nwverse/persona/server/internal/sessionlist"
)

func TestPagedSourceFailureDropsStaleRows(t *testing.T) {
	a := &memSource{name: "first", sessions: []AgentSession{{SessionKey: "a"}}}
	b := &memSource{name: "second", sessions: []AgentSession{{SessionKey: "b"}}}
	s := NewService(nil, nil, nil, a, b)
	first, err := s.ListPage(context.Background(), sessionlist.Options{})
	if err != nil || first.Total != 2 {
		t.Fatalf("baseline: %+v %v", first, err)
	}
	b.fail = errors.New("demo source unavailable")
	s.Updated("second", nil)
	page, err := s.ListPage(context.Background(), sessionlist.Options{Since: first.Revision})
	if err != nil || page.Total != 1 || len(page.Remove) != 1 || page.Remove[0] != "second:b" {
		t.Fatalf("failed source: %+v %v", page, err)
	}
}

func TestPagedCanceledRequestKeepsIndexedRows(t *testing.T) {
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	a := &memSource{name: "first", sessions: []AgentSession{{SessionKey: "a"}}}
	b := &memSource{name: "second", sessions: []AgentSession{{SessionKey: "b"}}}
	s := NewService(nil, nil, logf, a, b)
	first, err := s.ListPage(context.Background(), sessionlist.Options{})
	if err != nil || first.Total != 2 {
		t.Fatalf("baseline: %+v %v", first, err)
	}
	b.fail = context.Canceled
	s.cache.invalidate("second", nil)
	if _, err := s.ListPage(context.Background(), sessionlist.Options{Since: first.Revision}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ListPage: %v", err)
	}
	if _, err := s.ListCounts(context.Background(), sessionlist.Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ListCounts: %v", err)
	}
	if _, err := s.Sessions(context.Background(), "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Sessions: %v", err)
	}
	if _, err := s.Search(context.Background(), "", "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Search: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("canceled requests logged as source failures: %v", logs)
	}
	b.fail = nil
	page, err := s.ListPage(context.Background(), sessionlist.Options{Since: first.Revision})
	if err != nil || page.Total != 2 || len(page.Remove) != 0 || page.Revision != first.Revision {
		t.Fatalf("indexed rows dropped on cancel: %+v %v (was %s)", page, err, first.Revision)
	}
}

func TestReturnedSourceFailureIsLogged(t *testing.T) {
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	lone := &memSource{name: "lone", sessions: []AgentSession{{SessionKey: "a"}}}
	s := NewService(nil, nil, logf, lone)
	if _, err := s.ListPage(context.Background(), sessionlist.Options{}); err != nil {
		t.Fatal(err)
	}
	down := errors.New("gateway unreachable")
	lone.fail = down
	s.cache.invalidate("lone", nil)
	if _, err := s.ListPage(context.Background(), sessionlist.Options{}); !errors.Is(err, down) {
		t.Fatalf("ListPage: %v", err)
	}
	if _, err := s.Sessions(context.Background(), "", ""); !errors.Is(err, down) {
		t.Fatalf("Sessions: %v", err)
	}
	if _, err := s.Search(context.Background(), "", "a"); !errors.Is(err, down) {
		t.Fatalf("Search: %v", err)
	}
	for _, want := range []string{
		"agents: lone: paginated sessions: gateway unreachable",
		"agents: lone: sessions: gateway unreachable",
		"agents: lone: search: gateway unreachable",
	} {
		if !strings.Contains(strings.Join(logs, "\n"), want) {
			t.Fatalf("missing log %q in %v", want, logs)
		}
	}
}

func TestPagedLateInvalidatedSourceDoesNotResurrect(t *testing.T) {
	source := &memSource{name: "demo", sessions: []AgentSession{{SessionKey: "stale"}}}
	s := NewService(nil, nil, nil, source)
	source.during = func() { s.Updated("demo", nil) }
	page, err := s.ListPage(context.Background(), sessionlist.Options{})
	if err != nil || page.Total != 0 {
		t.Fatalf("stale list published: %+v %v", page, err)
	}
	source.during = nil
	source.sessions = []AgentSession{{SessionKey: "fresh"}}
	page, err = s.ListPage(context.Background(), sessionlist.Options{})
	if err != nil || page.Total != 1 {
		t.Fatalf("fresh list missing: %+v %v", page, err)
	}
}

func TestPagedRemovedSourceDoesNotResurrect(t *testing.T) {
	a := &memSource{name: "first", sessions: []AgentSession{{SessionKey: "a"}}}
	b := &memSource{name: "second", sessions: []AgentSession{{SessionKey: "b"}}}
	s := NewService(nil, nil, nil, a, b)
	a.during = func() { s.Remove("second") }
	page, err := s.ListPage(context.Background(), sessionlist.Options{})
	if err != nil || page.Total != 1 {
		t.Fatalf("removed source listed: %+v %v", page, err)
	}
	a.during = nil
	counts, err := s.ListCounts(context.Background(), sessionlist.Options{})
	if err != nil || counts.Total != 1 {
		t.Fatalf("removed source counted: %+v %v", counts, err)
	}
}

// The runtime palette's grammar is not a new spelling for a personal source's
// message search. Reject it before a provider is consulted instead of pretending
// its title-only index implements runtime operators and match evidence.
func TestPersonalAgentsRejectRuntimeSearch(t *testing.T) {
	source := &memSource{name: "example", fail: errors.New("provider should not be called")}
	service := NewService(nil, nil, nil, source)
	opts := sessionlist.Options{Search: "is:pinned needle"}
	for _, read := range []func() error{
		func() error { _, err := service.ListPage(context.Background(), opts); return err },
		func() error { _, err := service.ListPageWithCounts(context.Background(), opts); return err },
		func() error { _, err := service.ListCounts(context.Background(), opts); return err },
	} {
		var typed *sessionlist.Error
		if err := read(); !errors.As(err, &typed) || typed.Code != "invalid_query" {
			t.Fatalf("runtime search reached personal provider: %v", err)
		}
	}
}

// pinFixture is one source of n sessions, s0 the newest and s<n-1> the oldest.
func pinFixture(n int) (*Service, *[][]string) {
	sessions := make([]AgentSession, n)
	for i := range sessions {
		sessions[i] = AgentSession{SessionKey: fmt.Sprintf("s%d", i), RankTimeMs: int64(10_000 - i)}
	}
	var changes [][]string
	return NewService(nil, func(ids []string) { changes = append(changes, ids) }, nil,
		&memSource{name: "demo", sessions: sessions}), &changes
}

func pageIDs(t *testing.T, p sessionlist.Page) []string {
	t.Helper()
	ids := make([]string, len(p.Sessions))
	for i, raw := range p.Sessions {
		var row Session
		if err := json.Unmarshal(raw.(json.RawMessage), &row); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		ids[i] = row.ID
	}
	return ids
}

func firstPage(t *testing.T, s *Service, limit int) []string {
	t.Helper()
	p, err := s.ListPage(context.Background(), sessionlist.Options{Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	return pageIDs(t, p)
}

func TestPinnedSessionsLeadThePageInPinOrder(t *testing.T) {
	s, _ := pinFixture(5)
	if got := firstPage(t, s, 5); !slices.Equal(got, []string{"demo:s0", "demo:s1", "demo:s2", "demo:s3", "demo:s4"}) {
		t.Fatalf("unpinned order: %v", got)
	}
	if _, err := s.PutPrefs(context.Background(), Prefs{Pins: []string{"demo:s3", "demo:s1"}}); err != nil {
		t.Fatal(err)
	}
	if got := firstPage(t, s, 5); !slices.Equal(got, []string{"demo:s3", "demo:s1", "demo:s0", "demo:s2", "demo:s4"}) {
		t.Fatalf("pinned rows must lead in pin order, the rest newest first: %v", got)
	}
}

func TestPinnedOldSessionReachesTheFirstPage(t *testing.T) {
	s, _ := pinFixture(120)
	if slices.Contains(firstPage(t, s, 50), "demo:s119") {
		t.Fatal("fixture: the oldest row should start past the first page")
	}
	if _, err := s.PutPrefs(context.Background(), Prefs{Pins: []string{"demo:s119"}}); err != nil {
		t.Fatal(err)
	}
	p, err := s.ListPageWithCounts(context.Background(), sessionlist.Options{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	got := pageIDs(t, p.Page)
	if got[0] != "demo:s119" || got[1] != "demo:s0" || len(got) != 50 || p.Counts.Total != 120 {
		t.Fatalf("pinned oldest row must top page 1: %v (total %d)", got[:3], p.Counts.Total)
	}
	// the rest of the list still pages through every other row once
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	for cursor := p.NextCursor; cursor != ""; {
		next, err := s.ListPage(context.Background(), sessionlist.Options{Limit: 50, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range pageIDs(t, next) {
			if seen[id] {
				t.Fatalf("%s listed twice", id)
			}
			seen[id] = true
		}
		cursor = next.NextCursor
	}
	if len(seen) != 120 {
		t.Fatalf("paged %d rows, want 120", len(seen))
	}
}

func TestPinChangeReordersAtOnceAndTellsTheApps(t *testing.T) {
	s, changes := pinFixture(4)
	base, err := s.ListPage(context.Background(), sessionlist.Options{})
	if err != nil {
		t.Fatal(err)
	}
	*changes = nil
	if _, err := s.PutPrefs(context.Background(), Prefs{Pins: []string{"demo:s2"}}); err != nil {
		t.Fatal(err)
	}
	if len(*changes) != 1 {
		t.Fatalf("a pin change must tell the apps once: %v", *changes)
	}
	// the index is reordered without waiting for a source refresh: a delta already carries the pinned row
	delta, err := s.listIndex.Query(sessionlist.Options{Since: base.Revision})
	if err != nil || !slices.Equal(pageIDs(t, delta), []string{"demo:s2"}) {
		t.Fatalf("delta after pin: %v %v", delta, err)
	}
	full, err := s.listIndex.Query(sessionlist.Options{})
	if err != nil || pageIDs(t, full)[0] != "demo:s2" {
		t.Fatalf("index not reordered: %v %v", full, err)
	}
	// saving the same pins (a view change) does not reorder or notify
	*changes = nil
	if _, err := s.PutPrefs(context.Background(), Prefs{Pins: []string{"demo:s2"}, View: View{Sort: "name"}}); err != nil {
		t.Fatal(err)
	}
	if len(*changes) != 0 {
		t.Fatalf("unchanged pins notified: %v", *changes)
	}
	// unpinning puts the row back in its time order
	if _, err := s.PutPrefs(context.Background(), Prefs{Pins: []string{}}); err != nil {
		t.Fatal(err)
	}
	if got := firstPage(t, s, 4); !slices.Equal(got, []string{"demo:s0", "demo:s1", "demo:s2", "demo:s3"}) {
		t.Fatalf("unpinned row must go back to its place: %v", got)
	}
	if len(*changes) != 1 {
		t.Fatalf("unpin must tell the apps: %v", *changes)
	}
}

func TestPinKeepsHiddenSessionsOut(t *testing.T) {
	s, _ := pinFixture(3)
	firstPage(t, s, 3) // fill the cache Hide reads
	if _, err := s.Hide(context.Background(), "demo:s1"); err != nil {
		t.Fatal(err)
	}
	p, err := s.PutPrefs(context.Background(), Prefs{Pins: []string{"demo:s1", "demo:s2"}})
	if err != nil || p.Hidden["demo:s1"] == 0 {
		t.Fatalf("PutPrefs dropped hidden: %+v %v", p, err)
	}
	if got := firstPage(t, s, 3); !slices.Equal(got, []string{"demo:s2", "demo:s0"}) {
		t.Fatalf("hidden pinned row must stay out, the visible pin lead: %v", got)
	}
}
