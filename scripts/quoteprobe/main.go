// Command quoteprobe is the free probe of docs/SPEC_FASE6_CITAZIONI.md (work 6.0): on the
// answers of a finished bench round, it rebuilds each conversation (system prompt, question,
// one read per page, answer), sends the request "where does each sentence come from" to a
// model outside Vedetta, and looks for each passage the model copies on the pages.
//
// It talks only to the endpoint given with -base-url (halogen): no paid service. It reads
// the bench database and the round's cache read only, and writes scripts/out/quoteprobe_NAME.json
// (the round, the model, the endpoint and the time, then the cases).
//
//	go run ./scripts/quoteprobe -round known34_1 -base-url http://HOST:PORT/v1 -model NAME -limit 1
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"vedetta/internal/agent"
	"vedetta/internal/llm"
)

type caseResult struct {
	Case     string            `json:"case"`
	Pages    int               `json:"pages"`
	Missing  []string          `json:"pages_not_in_cache,omitempty"`
	Asked    int               `json:"asked"`
	Lines    int               `json:"lines"`
	Seconds  float64           `json:"seconds"`
	Error    string            `json:"error,omitempty"`
	Reply    string            `json:"reply"`
	Items    []agent.QuoteItem `json:"items"`
	Question string            `json:"question"`
}

func main() {
	round := flag.String("round", "known34_1", "bench round name (runs_NAME.json in -out)")
	out := flag.String("out", "scripts/out", "folder with the round files; the result goes here")
	home, _ := os.UserHomeDir()
	db := flag.String("db", filepath.Join(home, "vedetta-bench/data/vedetta.db"), "bench database, opened read only")
	cacheRoot := flag.String("cache-root", filepath.Join(home, "vedetta-bench/data"), "folder with cache-live-NAME")
	base := flag.String("base-url", "", "OpenAI-compatible endpoint of the local engine (required)")
	model := flag.String("model", "", "model name (required)")
	keyEnv := flag.String("api-key-env", "QUOTEPROBE_API_KEY", "environment variable holding the key, if the endpoint wants one")
	limit := flag.Int("limit", 0, "only the first N cases (0 = all)")
	only := flag.String("case", "", "only this case")
	flag.Parse()
	if *base == "" || *model == "" {
		fmt.Fprintln(os.Stderr, "-base-url and -model are required: the probe talks only to the engine you name")
		os.Exit(2)
	}

	var runs map[string]string
	mustJSON(filepath.Join(*out, "runs_"+*round+".json"), &runs)
	cases := make([]string, 0, len(runs))
	for c := range runs {
		if *only == "" || *only == c {
			cases = append(cases, c)
		}
	}
	sort.Strings(cases)
	if *limit > 0 && len(cases) > *limit {
		cases = cases[:*limit]
	}
	cache := loadCache(filepath.Join(*cacheRoot, "cache-live-"+*round))
	conn, err := sql.Open("sqlite", "file:"+*db+"?mode=ro")
	check(err)
	defer conn.Close()
	client := llm.NewClient(*base, os.Getenv(*keyEnv), *model)

	var results []caseResult
	for _, c := range cases {
		r := runCase(conn, client, c, runs[c], cache)
		results = append(results, r)
		fmt.Fprintf(os.Stderr, "%-28s pages=%d asked=%d lines=%d %.0fs %s\n", c, r.Pages, r.Asked, r.Lines, r.Seconds, r.Error)
	}
	file := filepath.Join(*out, "quoteprobe_"+*round+".json")
	data, err := probeFile(*round, *model, *base, time.Now(), results)
	check(err)
	check(os.WriteFile(file, data, 0o644))
	report(results, *out)
	fmt.Println("\nsaved", file)
}

// probeFile is the file of the results: the round, which model on which endpoint and when, then the cases.
func probeFile(round, model, baseURL string, when time.Time, cases []caseResult) ([]byte, error) {
	return json.MarshalIndent(struct {
		Round   string       `json:"round"`
		Model   string       `json:"model"`
		BaseURL string       `json:"base_url"`
		When    string       `json:"when"`
		Cases   []caseResult `json:"cases"`
	}{round, model, baseURL, when.Format(time.RFC3339), cases}, "", " ")
}

func runCase(conn *sql.DB, client *llm.Client, name, sid string, cache map[string]doc) caseResult {
	r := caseResult{Case: name}
	var question, answer, metaJSON string
	if err := conn.QueryRow(`SELECT query, content, COALESCE(meta_json,'{}') FROM messages WHERE session_id = ? AND role = 'assistant' ORDER BY created_at DESC LIMIT 1`, sid).Scan(&question, &answer, &metaJSON); err != nil {
		r.Error = "no answer: " + err.Error()
		return r
	}
	r.Question = question
	var meta struct {
		Loop struct {
			Calls []struct{ Tool, URL, Result string } `json:"calls_detail"`
		} `json:"loop"`
	}
	_ = json.Unmarshal([]byte(metaJSON), &meta)
	var pages []agent.QuoteProbePage
	seen := map[string]bool{}
	for _, c := range meta.Loop.Calls {
		if c.Tool != "read" || c.Result != "ok" || c.URL == "" || seen[norm(c.URL)] {
			continue
		}
		seen[norm(c.URL)] = true
		d, ok := cache[norm(c.URL)]
		if !ok {
			r.Missing = append(r.Missing, c.URL)
			continue
		}
		pages = append(pages, agent.QuoteProbePage{URL: c.URL, Title: d.Title, Text: d.Content})
	}
	r.Pages = len(pages)
	p := agent.NewQuoteProbe(question, answer, pages)
	r.Asked = len(p.Units)
	if r.Asked == 0 {
		r.Error = "nothing to ask"
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var reply strings.Builder
	t0 := time.Now()
	err := client.StreamChatWithOptions(ctx, p.Messages, llm.ChatOptions{Temperature: llm.Float64Ptr(0), Effort: llm.EffortOff},
		func(tok llm.StreamToken) error {
			if tok.Type == llm.TokenContent {
				reply.WriteString(tok.Text)
			}
			return nil
		})
	r.Seconds = time.Since(t0).Seconds()
	r.Reply = reply.String()
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Items, r.Lines = p.Settle(r.Reply)
	return r
}

func report(rs []caseResult, out string) {
	var asked, lines, refs, refsFound, found, notFound, short, none, foundNums, foundClean, moved int
	var secs []float64
	for _, r := range rs {
		asked += r.Asked
		lines += r.Lines
		if r.Seconds > 0 && r.Error == "" {
			secs = append(secs, r.Seconds)
		}
		for _, it := range r.Items {
			switch it.State {
			case "found":
				found++
				if agent.QuoteProbeHasNumbers(it.Sentence) {
					foundNums++
					if len(it.NumbersOut) == 0 {
						foundClean++
					}
				}
			case "not_found":
				notFound++
			case "short":
				short++
			case "none":
				none++
			}
			for _, ref := range it.Refs {
				refs++
				if ref.Found {
					refsFound++
					if ref.Moved {
						moved++
					}
				}
			}
		}
	}
	sort.Float64s(secs)
	med, max := 0.0, 0.0
	if len(secs) > 0 {
		med, max = secs[len(secs)/2], secs[len(secs)-1]
	}
	fmt.Println("\n| Riga | Valore |\n|---|---|")
	fmt.Printf("| 1. Formato: righe leggibili / frasi chieste | %s |\n", pct(lines, asked))
	fmt.Printf("| 2. Passi veri: trovati / indicati | %s |\n", pct(refsFound, refs))
	fmt.Printf("| 3. DECIDE: frasi found / chieste | %s (found %d, not_found %d, short %d, none %d) |\n", pct(found, asked), found, notFound, short, none)
	fmt.Printf("| 4. Numeri nel passo: found senza NumbersOut / found con numeri | %s |\n", pct(foundClean, foundNums))
	fmt.Printf("| 5. Pagina sbagliata: Moved / passi trovati | %s |\n", pct(moved, refsFound))
	fmt.Printf("| 6. Tempo: mediana / massimo (s) | %.0f / %.0f |\n", med, max)
	fmt.Println("| 7. Lettura a mano | vedi il file `quoteprobe_sample_*.json` (30 found, 15 none) |")
	writeSample(rs, out)
}

// writeSample saves 30 found and 15 none sentences, drawn with a fixed seed, for the hand reading of row 7.
func writeSample(rs []caseResult, out string) {
	type row struct {
		Case, Sentence, State, Question string
		Evidence                        []string
	}
	var found, none []row
	for _, r := range rs {
		for _, it := range r.Items {
			x := row{Case: r.Case, Sentence: it.Sentence, State: it.State}
			for _, ref := range it.Refs {
				if ref.Found {
					x.Evidence = append(x.Evidence, ref.Evidence+"  ["+ref.URL+"]")
				}
			}
			switch it.State {
			case "found":
				found = append(found, x)
			case "none":
				none = append(none, x)
			}
		}
	}
	rnd := rand.New(rand.NewSource(1))
	pick := func(in []row, n int) []row {
		rnd.Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })
		if len(in) > n {
			in = in[:n]
		}
		return in
	}
	sample := append(pick(found, 30), pick(none, 15)...)
	data, _ := json.MarshalIndent(sample, "", " ")
	_ = os.WriteFile(filepath.Join(out, "quoteprobe_sample_"+fmt.Sprint(len(rs))+".json"), data, 0o644)
}

func pct(a, b int) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%% (%d/%d)", 100*float64(a)/float64(b), a, b)
}

type doc struct{ Title, Content string }

// loadCache reads the pages of a round's cache folder: normalized URL -> document.
func loadCache(dir string) map[string]doc {
	out := map[string]doc{}
	files, _ := filepath.Glob(filepath.Join(dir, "page", "*", "*.json"))
	for _, f := range files {
		var e struct {
			Key      string `json:"key"`
			Document *doc   `json:"document"`
		}
		b, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(b, &e) != nil || e.Document == nil || !strings.HasPrefix(e.Key, "v1|page|") {
			continue
		}
		out[norm(strings.TrimPrefix(e.Key, "v1|page|"))] = *e.Document
	}
	return out
}

// norm is the address without scheme, fragment and final slash, lower-cased.
func norm(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	if i := strings.Index(u, "#"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return strings.TrimRight(u, "/.,;:")
}

func mustJSON(path string, v any) {
	b, err := os.ReadFile(path)
	check(err)
	check(json.Unmarshal(b, v))
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
