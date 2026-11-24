// Command claimnumbers runs Vedetta's number checks on saved texts, for the bench.
//
// Default: the A1 check of the extractor (docs/SPEC_QA_PACCHETTO_ONESTA.md §2, A1.1).
// Reads JSON lines {"id", "claim", "evidence", "question"} on stdin and writes JSON lines
// {"id", "missing"}: the claim numbers that are not in the evidence.
//
// With -loop: the check of the Deep a ciclo (Piano dopo audit 5.3). "claim" holds one
// sentence per line; "missing" lists the numbers of those sentences that are not in the
// evidence, with the roundings the Deep a ciclo accepts.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"vedetta/internal/agent"
)

type fact struct {
	ID       string `json:"id"`
	Claim    string `json:"claim"`
	Evidence string `json:"evidence"`
	Question string `json:"question"`
}

type verdict struct {
	ID      string   `json:"id"`
	Missing []string `json:"missing"`
}

func main() {
	loop := flag.Bool("loop", false, "the check of the Deep a ciclo, in place of the extractor's")
	flag.Parse()
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 64<<20) // the evidence may be all the pages of a conversation
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var f fact
		if err := json.Unmarshal(in.Bytes(), &f); err != nil {
			fmt.Fprintln(os.Stderr, "bad line:", err)
			os.Exit(1)
		}
		missing := []string{}
		if *loop {
			for _, m := range agent.MissingAnswerNumbers(f.Claim, f.Evidence) {
				missing = append(missing, m.Missing...)
			}
		} else if got := agent.MissingClaimNumbers(f.Claim, f.Evidence, f.Question); got != nil {
			missing = got
		}
		_ = out.Encode(verdict{ID: f.ID, Missing: missing})
	}
	if err := in.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
