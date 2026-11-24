package agent

import "strings"

// repetitionDetector detects degenerate generation loops (the same short phrase
// repeated many times) in a streaming text channel. It keeps a bounded tail and
// checks for periodic structure every `checkEvery` bytes, so it is cheap and does
// not need to scan the whole output.
//
// This is orthogonal to the *stall* watchdog (no tokens / no content): a model can
// happily emit tokens forever while repeating itself, which no stall detector sees.
type repetitionDetector struct {
	tail       []byte
	checkedAt  int
	total      int
	checkEvery int
	minPeriod  int
	maxPeriod  int
	minRepeats int
}

func newRepetitionDetector() *repetitionDetector {
	return &repetitionDetector{
		checkEvery: 400,
		minPeriod:  10,
		// A loop can be a whole list of lines, not one phrase: case 6 of
		// 28/10/2026 cycled 10 lines with a period of 522 bytes.
		maxPeriod:  2000,
		minRepeats: 4,
	}
}

// Write appends a chunk and reports whether a repetition loop is present.
func (d *repetitionDetector) Write(s string) bool {
	d.total += len(s)
	d.tail = append(d.tail, s...)
	// The tail must hold minRepeats copies of the longest period.
	maxTail := d.maxPeriod*d.minRepeats + d.checkEvery
	if len(d.tail) > maxTail {
		d.tail = d.tail[len(d.tail)-maxTail:]
	}
	if d.total-d.checkedAt < d.checkEvery {
		return false
	}
	d.checkedAt = d.total
	return detectRepetition(string(d.tail), d.minPeriod, d.maxPeriod, d.minRepeats)
}

// detectRepetition reports whether the end of s consists of minRepeats consecutive
// identical segments whose length is between minPeriod and maxPeriod bytes.
func detectRepetition(s string, minPeriod, maxPeriod, minRepeats int) bool {
	n := len(s)
	if n < minPeriod*minRepeats {
		return false
	}
	maxP := maxPeriod
	if maxP > n/minRepeats {
		maxP = n / minRepeats
	}
	for p := minPeriod; p <= maxP; p++ {
		if n < p*minRepeats {
			break
		}
		last := s[n-p:]
		if strings.TrimSpace(last) == "" {
			continue
		}
		ok := true
		for r := 1; r < minRepeats; r++ {
			seg := s[n-(r+1)*p : n-r*p]
			if seg != last {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
