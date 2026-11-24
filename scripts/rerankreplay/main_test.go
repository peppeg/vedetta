package main

import (
	"testing"

	"vedetta/internal/cache"
	"vedetta/internal/search"
)

func results(urls ...string) []search.Result {
	out := make([]search.Result, len(urls))
	for i, u := range urls {
		out[i] = search.Result{URL: u, Title: u}
	}
	return out
}

// The counts of one search before and after the reorder (Piano dopo audit 2.3).
func TestCount(t *testing.T) {
	var st stats
	before := results("https://a.test/1", "https://b.test/2", "https://c.test/3", "https://www.official.test/home")
	after := results("https://www.official.test/home", "https://a.test/1", "https://b.test/2") // c removed
	st.count(before, after, []string{"official.test"})
	want := stats{Searches: 1, Results: 4, Removed: 1, LostOne: 1, FirstChanged: 1, Top3Changed: 1,
		HomePresent: 1, HomeUp: 1, HomePosBefore: 4, HomePosAfter: 1, HomeKept: 1, HomeTop3After: 1}
	// a.test, the engine's first, is second: still among the first three.
	if st != want {
		t.Errorf("stats\n got %+v\nwant %+v", st, want)
	}

	var same stats
	same.count(before, before, []string{"official.test"})
	if same.Removed != 0 || same.FirstChanged != 0 || same.Top3Changed != 0 || same.HomeUp+same.HomeDown != 0 || same.HomeTop3Before != 0 {
		t.Errorf("an unchanged order counts changes: %+v", same)
	}

	var gone stats
	gone.count(before, results("https://b.test/2"), []string{"official.test"})
	if gone.FirstRemoved != 1 || gone.FirstOutOfTop3 != 1 || gone.HomeRemoved != 1 || gone.HomeKept != 0 {
		t.Errorf("removed first result or home not counted: %+v", gone)
	}

	var pushed stats
	pushed.count(before, results("https://b.test/2", "https://c.test/3", "https://www.official.test/home", "https://a.test/1"), nil)
	if pushed.FirstOutOfTop3 != 1 || pushed.FirstRemoved != 0 || pushed.Top3Changed != 1 {
		t.Errorf("the engine's first pushed to fourth: %+v", pushed)
	}
}

// The same search in several rounds' caches counts once (as the audit counted).
func TestReplayCountsEachSearchOnce(t *testing.T) {
	var st stats
	r := newReplay(nil, nil, false)
	e := cache.Entry{Key: `v1|search|searxng|"komodo release"|lang=it|cat=|max=10|bio=false|auth=`, Results: results("https://a.test/1", "https://b.test/2")}
	r.questions = map[string]string{"komodo release": "Qual è l'ultima versione di Komodo?"}
	r.rr = keepOrder{}
	r.add(e)
	r.add(e)
	st = r.with
	if st.Searches != 1 || r.duplicates != 1 {
		t.Errorf("searches %d duplicates %d, want 1 and 1", st.Searches, r.duplicates)
	}
}

type keepOrder struct{}

func (keepOrder) Rerank(q string, res []search.Result, min float64) []search.Result { return res }
func (keepOrder) RerankWithRoot(q, root string, res []search.Result, min float64) []search.Result {
	return res
}

func TestQueryOfKey(t *testing.T) {
	for key, want := range map[string]string{
		`v1|search|searxng|"komodo release"|lang=it|cat=|max=10|bio=false|auth=`:        "komodo release",
		`v1|search|searxng|"\"exact phrase\" more"|lang=en|cat=|max=10|bio=false|auth=`: `"exact phrase" more`,
		`v1|search|searxng|"città | prezzi"|lang=it|cat=general|max=10|bio=false|auth=`: "città | prezzi",
	} {
		if got, ok := queryOfKey(key); !ok || got != want {
			t.Errorf("queryOfKey(%q) = %q, %t; want %q", key, got, ok, want)
		}
	}
	if _, ok := queryOfKey("v1|page|https://a.test/"); ok {
		t.Error("a page key read as a search")
	}
}
