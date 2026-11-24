// Command rerankreplay redoes the reorder of the search results on the searches
// saved in the cache, and counts what it changes (Piano dopo audit 2.3, audit D8).
// The cache holds the results as the search service gave them, before the reorder,
// so the replay is free and repeatable.
//
//	rerankreplay [-db PATH] [-expected FILE] [-noroot] [CACHE_DIR ...]
//
// Each search counts once, even when several rounds' caches hold it.
//
// The reorder is the production one: the intent and authority reranker over BM25,
// with the configuration's default options (flags below), called as the Deep loop
// does, RerankWithRoot(search, user's question, results, 0.05). The user's question
// is not in the cache: it comes from the database, through the searches each answer
// saved in meta.loop.calls_detail. A search no answer contains is counted apart and
// left out of the numbers (with -noroot it is replayed without a question).
//
// Default: the bench database and every cache-live-* folder of the bench.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"

	"vedetta/internal/cache"
	"vedetta/internal/search"
)

// stats are the counts over the searches replayed. Positions are 1-based sums.
type stats struct {
	Searches, Results, Removed                 int
	LostOne, FirstRemoved, FirstChanged        int
	FirstOutOfTop3                             int // the engine's first result is no longer among the first three (the audit's row)
	Top3Changed                                int // a result among the engine's first three is no longer among the first three
	HomePresent, HomeRemoved, HomeDown, HomeUp int // official pages of expected.json among the results
	HomeKept                                   int
	HomePosBefore, HomePosAfter                int // sums over the kept ones
	HomeTop3Before, HomeTop3After              int
}

// normURL is eval.py's norm_url: no scheme, no www, lower-case host, no fragment
// and no final slash.
func normURL(u string) string {
	u = strings.TrimRight(strings.TrimSpace(u), ".,;:!?")
	low := strings.ToLower(u)
	for _, p := range []string{"https://", "http://"} {
		if strings.HasPrefix(low, p) {
			u, low = u[len(p):], low[len(p):]
			break
		}
	}
	if strings.HasPrefix(low, "www.") {
		u = u[4:]
	}
	u, _, _ = strings.Cut(u, "#")
	host, path, found := strings.Cut(u, "/")
	out := strings.ToLower(host)
	if found {
		out += "/" + path
	}
	return strings.TrimRight(out, "/")
}

func isHome(u string, homes []string) bool {
	n := normURL(u)
	for _, h := range homes {
		if h != "" && strings.Contains(n, strings.TrimPrefix(strings.ToLower(h), "www.")) {
			return true
		}
	}
	return false
}

// count adds one search: its results before and after the reorder.
func (st *stats) count(before, after []search.Result, homes []string) {
	st.Searches++
	st.Results += len(before)
	pos := make(map[string]int, len(after))
	for i, r := range after {
		if _, seen := pos[normURL(r.URL)]; !seen {
			pos[normURL(r.URL)] = i + 1
		}
	}
	removed := 0
	for _, r := range before {
		if _, ok := pos[normURL(r.URL)]; !ok {
			removed++
		}
	}
	st.Removed += removed
	if removed > 0 {
		st.LostOne++
	}
	if len(before) == 0 {
		return
	}
	if _, ok := pos[normURL(before[0].URL)]; !ok {
		st.FirstRemoved++
	}
	if len(after) == 0 || normURL(after[0].URL) != normURL(before[0].URL) {
		st.FirstChanged++
	}
	if p, ok := pos[normURL(before[0].URL)]; !ok || p > 3 {
		st.FirstOutOfTop3++
	}
	for i := 0; i < len(before) && i < 3; i++ {
		if p, ok := pos[normURL(before[i].URL)]; !ok || p > 3 {
			st.Top3Changed++
			break
		}
	}
	for i, r := range before {
		if !isHome(r.URL, homes) {
			continue
		}
		st.HomePresent++
		if i < 3 {
			st.HomeTop3Before++
		}
		p, ok := pos[normURL(r.URL)]
		if !ok {
			st.HomeRemoved++
			continue
		}
		st.HomeKept++
		st.HomePosBefore += i + 1
		st.HomePosAfter += p
		switch {
		case p > i+1:
			st.HomeDown++
		case p < i+1:
			st.HomeUp++
		}
		if p <= 3 {
			st.HomeTop3After++
		}
	}
}

var searchKeyRe = regexp.MustCompile(`^v\d+\|search\|[^|]*\|(".*")\|lang=`)

// queryOfKey reads the search out of a cache key (cache.SearchKey writes it with %q).
func queryOfKey(key string) (string, bool) {
	m := searchKeyRe.FindStringSubmatch(key)
	if m == nil {
		return "", false
	}
	q, err := strconv.Unquote(m[1])
	return q, err == nil
}

// normQuery is cache.normalizeQuery: lower case, single spaces.
func normQuery(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

// roots maps every search the Deep loop answers saved to the user's question.
func roots(db string) (map[string]string, error) {
	conn, err := sql.Open("sqlite", "file:"+db+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := conn.Query(`SELECT query, meta_json FROM messages WHERE meta_json LIKE '%calls_detail%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var question, meta string
		if err := rows.Scan(&question, &meta); err != nil {
			return nil, err
		}
		var m struct {
			Loop struct {
				Calls []struct{ Tool, Query string } `json:"calls_detail"`
			} `json:"loop"`
		}
		if json.Unmarshal([]byte(meta), &m) != nil {
			continue
		}
		for _, c := range m.Loop.Calls {
			if k := normQuery(c.Query); c.Tool == "search" && k != "" && out[k] == "" {
				out[k] = question
			}
		}
	}
	return out, rows.Err()
}

func homes(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e struct {
		Cases map[string]struct {
			Entities []struct {
				Home []string `json:"home"`
			} `json:"entities"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, err
	}
	var out []string
	for _, c := range e.Cases {
		for _, ent := range c.Entities {
			out = append(out, ent.Home...)
		}
	}
	return out, nil
}

// replay reorders the cached searches, each search once even when several rounds'
// caches hold it (the audit counted them so).
type replay struct {
	questions  map[string]string
	homes      []string
	rr         search.RootAwareReranker
	noRoot     bool
	seen       map[string]bool
	with       stats // searches an answer contains: reordered with the user's question
	without    stats // the others, with -noroot
	unmatched  int
	duplicates int
}

func newReplay(questions map[string]string, homes []string, noRoot bool) *replay {
	return &replay{questions: questions, homes: homes, noRoot: noRoot, seen: map[string]bool{}}
}

func (r *replay) add(e cache.Entry) {
	q, ok := queryOfKey(e.Key)
	if !ok || len(e.Results) == 0 {
		return
	}
	if r.seen[e.Key] {
		r.duplicates++
		return
	}
	r.seen[e.Key] = true
	before := append([]search.Result(nil), e.Results...)
	question := r.questions[normQuery(q)]
	if question == "" {
		r.unmatched++
		if r.noRoot {
			r.without.count(before, r.rr.RerankWithRoot(q, "", append([]search.Result(nil), e.Results...), 0.05), r.homes)
		}
		return
	}
	r.with.count(before, r.rr.RerankWithRoot(q, question, append([]search.Result(nil), e.Results...), 0.05), r.homes)
}

func pct(n, d int) string {
	if d == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(d))
}

func mean(sum, n int) string {
	if n == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", float64(sum)/float64(n))
}

func main() {
	home, _ := os.UserHomeDir()
	db := flag.String("db", filepath.Join(home, "vedetta-bench/data/vedetta.db"), "database with the answers (the user's questions)")
	expected := flag.String("expected", "scripts/expected.json", "cases with the official pages (home)")
	noRoot := flag.Bool("noroot", false, "also replay the searches no answer contains, without a question")
	boost := flag.Float64("authority-boost", 0.30, "VEDETTA_AUTHORITY_BOOST")
	project := flag.Float64("project-penalty", 0.50, "VEDETTA_PROJECT_PENALTY")
	municipal := flag.Float64("municipal-penalty", 0.80, "VEDETTA_MUNICIPAL_PENALTY")
	preset := flag.String("preset", "", `"it" when VEDETTA_PRESET_IT is on`)
	flag.Parse()

	dirs := flag.Args()
	if len(dirs) == 0 {
		dirs, _ = filepath.Glob(filepath.Join(home, "vedetta-bench/data/cache-live-*"))
	}
	questions, err := roots(*db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rerankreplay: database:", err)
		os.Exit(1)
	}
	officials, err := homes(*expected)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rerankreplay: expected:", err)
		os.Exit(1)
	}
	opts := search.IntentRerankerOptions{AuthorityBoost: *boost, ProjectPenalty: *project, MunicipalPenalty: *municipal, ProjectBoost: 0.50, Preset: *preset}
	if *preset != "" {
		search.SetPresetActive(*preset, true)
	}

	r := newReplay(questions, officials, *noRoot)
	r.rr = search.NewIntentReranker(search.NewBM25Reranker(), opts)
	for _, dir := range dirs {
		_ = filepath.WalkDir(filepath.Join(dir, "search"), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			var e cache.Entry
			if json.Unmarshal(raw, &e) == nil {
				r.add(e)
			}
			return nil
		})
	}
	report := func(title string, s stats) {
		fmt.Printf("%s: %d searches, %d results\n", title, s.Searches, s.Results)
		fmt.Printf("  results removed                          %d (%s)\n", s.Removed, pct(s.Removed, s.Results))
		fmt.Printf("  searches that lose at least one result   %d (%s)\n", s.LostOne, pct(s.LostOne, s.Searches))
		fmt.Printf("  engine's first result removed            %d\n", s.FirstRemoved)
		fmt.Printf("  first result changed                     %d (%s)\n", s.FirstChanged, pct(s.FirstChanged, s.Searches))
		fmt.Printf("  engine's first no longer in the first 3  %d (%s)\n", s.FirstOutOfTop3, pct(s.FirstOutOfTop3, s.Searches))
		fmt.Printf("  one of engine's first 3 leaves the 3     %d (%s)\n", s.Top3Changed, pct(s.Top3Changed, s.Searches))
		fmt.Printf("  official pages among the results         %d: removed %d, down %d, up %d\n", s.HomePresent, s.HomeRemoved, s.HomeDown, s.HomeUp)
		fmt.Printf("  official pages, mean position (kept)     %s -> %s\n", mean(s.HomePosBefore, s.HomeKept), mean(s.HomePosAfter, s.HomeKept))
		fmt.Printf("  official pages among the first three     %d -> %d\n", s.HomeTop3Before, s.HomeTop3After)
	}
	fmt.Printf("caches: %d; the same search in several caches counted once (%d repeats); searches no answer contains: %d (left out)\n",
		len(dirs), r.duplicates, r.unmatched)
	report("with the user's question", r.with)
	if *noRoot {
		report("without a question (searches no answer contains)", r.without)
	}
}
