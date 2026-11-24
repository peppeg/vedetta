package agent

import (
	"math"
	"strconv"
	"strings"
	"unicode"
)

// Numbers of a fact must be in its evidence (V25-QA A1, T3.2). Both sides go
// through the same normalizer, so "1.000", "1,000", "1 000", "mille" and
// "thousand" are the same number, "44 mila" is 44000, "mille miliardi" is 10^12,
// "40 MHz" is also 40 million and "anni '80" is 1980. "migliaia di miliardi" is no
// number but a range, 10^12 to 10^15: any number of that size stands for it, and
// "thousands of trillions" (10^15) does not (case 19, 05/11/2026).
// Pure Go, no LLM: see docs/SPEC_QA_PACCHETTO_ONESTA.md §2.

// MissingClaimNumbers returns the numbers of claim that are neither in the
// evidence nor in the user's question (the claim may repeat the question: "3-day
// forecast"), in canonical form. Empty means the claim passes.
func MissingClaimNumbers(claim, evidence, question string) []string {
	return missingNumbers(claim, numberSet(evidence, question))
}

// numberSet holds the canonical forms of the numbers of texts, the evidence side
// of the check: built once, it serves many claims.
func numberSet(texts ...string) map[string]bool {
	have := make(map[string]bool)
	for _, text := range texts {
		for _, n := range scanNumbers(text) {
			for _, alt := range n.alts {
				have[alt] = true
				v, err := strconv.ParseFloat(alt, 64)
				if err == nil && v >= 1 {
					have[sizeKey(powerOfTen(v))] = true
				}
				// "117,432" stars may be written "117k" or "117.4k"
				if err == nil && v >= 1000 {
					have[kKey(math.Floor(v/1e3))] = true
					have[kKey(math.Round(v/1e3))] = true
					have[kKey(math.Floor(v/100)/10)] = true
					have[kKey(math.Round(v/100)/10)] = true
				}
			}
			if n.raw != "" {
				have[n.raw] = true
			}
		}
	}
	return have
}

// kKey is the form a number written with "k" is matched by: "k:117" for 117k.
func kKey(thousands float64) string {
	return "k:" + canonical(thousands)
}

// sizeKey is the form a vague quantity is matched by: "size:12" holds the numbers
// from 10^12 to just under 10^13.
func sizeKey(power int) string {
	return "size:" + strconv.Itoa(power)
}

// powerOfTen is the power of ten of the first digit of v, which is at least 1:
// counted on the digits, because math.Log10(1000) is 2.9999999999999996.
func powerOfTen(v float64) int {
	return len(strconv.FormatFloat(math.Trunc(v), 'f', 0, 64)) - 1
}

// missingNumbers returns the numbers of claim that are not in have.
func missingNumbers(claim string, have map[string]bool) []string {
	var missing []string
	for _, n := range scanNumbers(claim) {
		if n.weak || n.ident {
			continue // "one", "uno", months as words, "RC1": not required from a claim
		}
		found := false
		for _, alt := range n.alts {
			if have[alt] {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, n.label())
		}
	}
	return missing
}

// number is one number found in a text: alts are the canonical forms it may
// stand for (a decade "anni '20" is 1920 or 2020), raw the plain value before a
// multiplier ("44" in "44 mila"), accepted only on the evidence side.
type number struct {
	alts  []string
	raw   string
	weak  bool   // a word too common to require ("one", "uno", "May")
	ident bool   // digits glued to letters before them: part of a name ("RC1", "F0")
	vague string // the words of a quantity that is a range ("migliaia di miliardi"): alts are its sizes
}

// label is how a number is named when it is reported as missing.
func (n number) label() string {
	if n.vague != "" {
		return n.vague
	}
	return n.alts[0]
}

type numToken struct {
	word    string  // lower-case word, "" for a number written in digits
	value   float64 // value of a number written in digits
	digits  bool
	ndigits int    // digits written before the decimal part ("59" in "1954–59" has 2)
	suffix  string // letters glued after the digits ("s" in "1980s", "er" in "80er")
	prefixQ bool   // an apostrophe right before the digits ("'80")
	dash    bool   // a dash right before the digits ("–59" in "1954–59")
	ident   bool   // a letter right before the digits ("RC1")
	text    string // the digits as written, separators included ("1,6" or "1.500")
	apart   bool   // punctuation before it: it does not continue the number before ("30 milioni. Mille")
}

func scanNumbers(text string) []number {
	toks := tokenizeNumbers(text)
	lang := DetectTextLanguage(text)
	var out []number
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t.digits:
			v := t.value
			if t.ident {
				out = append(out, number{alts: []string{canonical(v)}, ident: true})
				continue
			}
			if t.suffix == "k" {
				// "117k" stars: a thousand times, rounded (kNumber)
				out = append(out, number{alts: []string{canonical(v * 1e3), kKey(v)}, raw: canonical(v)})
				continue
			}
			if d, ok := decadeOf(toks, i, lang); ok {
				out = append(out, d)
				continue
			}
			// "40 MHz" is also "40 milioni": the number stays as it is written, so
			// a small one is still not required, and the value is one more form of it
			if scale, ok := hertzScales[t.suffix]; ok {
				out = append(out, number{alts: []string{canonical(v), canonical(v * scale)}})
				continue
			}
			if i+1 < len(toks) && !toks[i+1].apart {
				if scale, ok := hertzScales[toks[i+1].word]; ok {
					out = append(out, number{alts: []string{canonical(v), canonical(v * scale)}})
					i++
					continue
				}
			}
			// "1954–59": the second year of a range written with two digits
			if t.dash && t.ndigits == 2 && i > 0 && toks[i-1].digits && toks[i-1].ndigits == 4 {
				full := math.Floor(toks[i-1].value/100)*100 + v
				if full > toks[i-1].value {
					out = append(out, number{alts: []string{canonical(full), canonical(v)}})
					continue
				}
			}
			// "44 mila", "44mila", "2 milioni di miliardi"
			if factors, next := scaleFactors(toks, i+1, scaleValues(t.suffix, lang), lang); factors != nil {
				out = append(out, number{alts: scaled(v, factors), raw: canonical(v)})
				i = next - 1
				continue
			}
			out = append(out, number{alts: []string{canonical(v)}})
		case t.word != "":
			if r, ok := romanCentury(toks, i); ok {
				out = append(out, number{alts: []string{strconv.Itoa(r)}})
				continue
			}
			if m, isMonth := monthWords[t.word]; isMonth {
				out = append(out, number{alts: []string{strconv.Itoa(m)}, weak: true})
				continue
			}
			v, ok := wordValue(t.word, lang)
			if !ok {
				if n, next, isVague := vagueNumber(toks, i, lang); isVague {
					if n.alts != nil {
						out = append(out, n)
					}
					i = next - 1
				} else if vs := scaleValues(t.word, lang); vs != nil {
					// "mille", "cento milioni", "mille miliardi"
					factors, next := scaleFactors(toks, i+1, vs, lang)
					out = append(out, number{alts: scaled(1, factors)})
					i = next - 1
				} else if d, isDec := decadeWords[t.word]; isDec {
					out = append(out, decadeNumber(d))
				}
				continue
			}
			if d, ok := decadeOf(toks, i, lang); ok {
				out = append(out, d)
				continue
			}
			// "twenty-three", "vingt-trois", "trente et un", "treinta y dos"
			if v >= 20 && v < 100 && int(v)%10 == 0 {
				j := i + 1
				if j < len(toks) && (toks[j].word == "et" || toks[j].word == "y" || toks[j].word == "and") {
					j++
				}
				if j < len(toks) {
					// after a tens word a false friend is a unit again ("treinta y dos")
					u, ok := wordValue(toks[j].word, lang)
					if !ok && lang == "" {
						u, ok = numberWords[toks[j].word]
					}
					if ok && u >= 1 && u <= 9 {
						v += u
						i = j
					}
				}
			}
			if factors, next := scaleFactors(toks, i+1, nil, lang); factors != nil {
				out = append(out, number{alts: scaled(v, factors)})
				i = next - 1
				continue
			}
			out = append(out, number{alts: []string{canonical(v)}, weak: v == 1})
		}
	}
	return out
}

// scaleValues is what a scale word multiplies by: more than one value when usage
// differs, none when word is not a scale word.
func scaleValues(word, lang string) []float64 {
	if vs, ok := ambiguousScales[word]; ok {
		return vs
	}
	if m, ok := multiplierWords[word]; ok {
		return []float64{m}
	}
	if word == "miles" && lang == "es" {
		return []float64{1e3} // in English a "mile" is a distance
	}
	return nil
}

// scaleFactors reads the scale words at toks[i:] and multiplies them onto factors:
// "milioni di miliardi", "mila miliardi". It returns the factors, nil if there are
// none, and the index after the last word read. After the first word only scales
// of a thousand or more continue the number: "mille cento" is no 100,000.
func scaleFactors(toks []numToken, i int, factors []float64, lang string) ([]float64, int) {
	for {
		j := i
		if factors != nil && j < len(toks) && scaleLinks[toks[j].word] && !toks[j].apart {
			j++
		}
		if j >= len(toks) || toks[j].apart {
			return factors, i
		}
		vs := scaleValues(toks[j].word, lang)
		if vs == nil || (factors != nil && vs[0] < 1e3) {
			return factors, i
		}
		if factors == nil {
			factors = vs
		} else {
			var product []float64
			for _, f := range factors {
				for _, v := range vs {
					product = append(product, f*v)
				}
			}
			factors = product
		}
		i = j + 1
	}
}

// scaled writes v times each factor. The rounding removes the float noise of the
// product: 4.35 million must read 4350000.
func scaled(v float64, factors []float64) []string {
	out := make([]string, 0, len(factors))
	for _, f := range factors {
		out = append(out, canonical(math.Round(v*f*1e6)/1e6))
	}
	return out
}

// vagueNumber reads a scale word in the plural with no number before it, which names
// a range: "milioni" goes from 10^6 to just under 10^9, "centinaia di migliaia" from
// 10^5 to just under 10^6. "miliardi" is not found in a page that only speaks of
// millions, but "migliaia" or "hundreds" alone fit almost anything on a page: those
// are kept for the evidence side and not required from a claim, like "one". "decine"
// alone says nothing and is no number; isVague is true and alts nil. It returns the
// index after the last word read.
func vagueNumber(toks []numToken, i int, lang string) (n number, next int, isVague bool) {
	w := toks[i].word
	vague, ok := vagueWords[w]
	if !ok || (w == "miles" && lang != "es") {
		return number{}, 0, false
	}
	base := scaleValues(w, lang)
	if base == nil {
		base = []float64{vague.value}
	}
	factors, next := scaleFactors(toks, i+1, base, lang)
	if next == i+1 && vague.value == 10 {
		return number{}, next, true
	}
	var alts []string
	for _, f := range factors {
		for d := 0; d < vague.span; d++ {
			alts = append(alts, sizeKey(powerOfTen(f)+d))
		}
	}
	words := make([]string, 0, next-i)
	for _, t := range toks[i:next] {
		words = append(words, t.word)
	}
	return number{alts: alts, vague: strings.Join(words, " "), weak: next == i+1 && factors[0] < 1e6}, next, true
}

// decadeOf reads "anni '80", "années 80", "años 80", "the '80s", "1980s",
// "80er (Jahre)" and "anni ottanta" at toks[i].
func decadeOf(toks []numToken, i int, lang string) (number, bool) {
	t := toks[i]
	v := t.value
	if t.word != "" {
		w, ok := wordValue(t.word, lang)
		if !ok {
			return number{}, false
		}
		v = w
	}
	if v != math.Trunc(v) {
		return number{}, false
	}
	n := int(v)
	if t.digits && n >= 1000 && n%10 == 0 && t.suffix == "s" {
		return number{alts: []string{strconv.Itoa(n)}}, true
	}
	if n < 10 || n > 90 || n%10 != 0 {
		return number{}, false
	}
	prev := ""
	if i > 0 {
		prev = toks[i-1].word
	}
	decade := t.suffix == "s" || t.suffix == "er"
	switch prev {
	case "anni", "années", "años", "jahre":
		decade = true
	case "the":
		decade = decade || t.prefixQ
	}
	if !decade {
		return number{}, false
	}
	return decadeNumber(n), true
}

func decadeNumber(n int) number {
	if n < 30 {
		return number{alts: []string{strconv.Itoa(1900 + n), strconv.Itoa(2000 + n), strconv.Itoa(n)}}
	}
	return number{alts: []string{strconv.Itoa(1900 + n), strconv.Itoa(n)}}
}

// romanCentury reads "XIX secolo", "siglo XIX", "XIXe siècle", "XIX. Jahrhundert".
func romanCentury(toks []numToken, i int) (int, bool) {
	w := strings.TrimSuffix(toks[i].word, "e")
	if w == "" || strings.Trim(w, "ivxlc") != "" {
		return 0, false
	}
	near := func(j int) bool { return j >= 0 && j < len(toks) && centuryWords[toks[j].word] }
	if !near(i-1) && !near(i+1) {
		return 0, false
	}
	r := romanValue(w)
	return r, r > 0 && r <= 30
}

func romanValue(s string) int {
	vals := map[byte]int{'i': 1, 'v': 5, 'x': 10, 'l': 50, 'c': 100}
	total := 0
	for i := 0; i < len(s); i++ {
		v := vals[s[i]]
		if i+1 < len(s) && vals[s[i+1]] > v {
			total -= v
		} else {
			total += v
		}
	}
	return total
}

// wordValue is the value of a number word in the language of the text. When the
// language is unknown, words that mean something else in another language
// ("due" in "due to", "seize", "dos") are not read as numbers.
func wordValue(w, lang string) (float64, bool) {
	v, ok := numberWords[w]
	if !ok {
		return 0, false
	}
	if lang == "" {
		return v, !falseFriends[w]
	}
	return v, wordLangs[w][lang]
}

// canonical writes a value without separators and trailing zeros: 1000, 12.5.
func canonical(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func isThousandsSep(r rune) bool {
	return r == '.' || r == ',' || r == ' ' || r == '\u00a0' || r == '\u202f' || r == '\u2009' || r == '\'' || r == '’'
}

// tokenizeNumbers splits text into numbers written in digits and lower-case words.
func tokenizeNumbers(text string) []numToken {
	var toks []numToken
	rs := []rune(text)
	apart := false
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsDigit(r):
			start := i
			intPart, j := readDigits(rs, i)
			// thousands groups: a separator followed by exactly 3 digits, only after 1-3 leading
			// digits, and not after a leading zero: "0,276" is a decimal, "030 115" a phone number
			usedSep := rune(0)
			if len(intPart) <= 3 && intPart[0] != '0' {
				for j < len(rs) && isThousandsSep(rs[j]) && (usedSep == 0 || rs[j] == usedSep) {
					g, k := readDigits(rs, j+1)
					if len(g) != 3 || (k < len(rs) && unicode.IsDigit(rs[k])) {
						break
					}
					usedSep = rs[j]
					intPart += g
					j = k
				}
			}
			frac := ""
			if j+1 < len(rs) && (rs[j] == '.' || rs[j] == ',') && rs[j] != usedSep && unicode.IsDigit(rs[j+1]) {
				// "27.6.2022" is a date, not the decimal 27.6: only its year is read, as in "27/6/2022"
				if next, isDate := dottedDate(rs, intPart, j); isDate {
					apart = true
					i = next
					continue
				}
				frac, j = readDigits(rs, j+1)
			}
			s := strings.TrimLeft(intPart, "0")
			if s == "" {
				s = "0"
			}
			if frac = strings.TrimRight(frac, "0"); frac != "" {
				s += "." + frac
			}
			v, _ := strconv.ParseFloat(s, 64)
			suffix := ""
			k := j
			for k < len(rs) && unicode.IsLetter(rs[k]) {
				k++
			}
			if k > j {
				suffix = strings.ToLower(string(rs[j:k]))
			}
			prefixQ := start > 0 && (rs[start-1] == '\'' || rs[start-1] == '’')
			dash := start > 0 && (rs[start-1] == '-' || rs[start-1] == '–' || rs[start-1] == '—')
			// "RC1" is a name; "v2.0" is a version, a number to check
			ident := start > 0 && unicode.IsLetter(rs[start-1]) &&
				!((rs[start-1] == 'v' || rs[start-1] == 'V') && (start == 1 || !unicode.IsLetter(rs[start-2])))
			toks = append(toks, numToken{value: v, digits: true, ndigits: len(intPart), suffix: suffix, prefixQ: prefixQ,
				dash: dash, ident: ident, apart: apart, text: string(rs[start:j])})
			apart = false
			i = k
		case unicode.IsLetter(r):
			start := i
			for i < len(rs) && unicode.IsLetter(rs[i]) {
				i++
			}
			w := strings.ToLower(string(rs[start:i]))
			toks = append(toks, numToken{word: w, apart: apart})
			apart = false
		default:
			apart = apart || strings.ContainsRune(".,;:!?()[]{}|\n", r)
			i++
		}
	}
	return toks
}

// dottedDate reports whether the digits of day, with the dot at dot after them, open
// a date "27.6.2022", and returns where its year starts.
func dottedDate(rs []rune, day string, dot int) (year int, isDate bool) {
	month, j := readDigits(rs, dot+1)
	if rs[dot] != '.' || len(day) > 2 || len(month) > 2 || j >= len(rs) || rs[j] != '.' {
		return 0, false
	}
	y, k := readDigits(rs, j+1)
	d, _ := strconv.Atoi(day)
	m, _ := strconv.Atoi(month)
	if len(y) != 4 || (k+1 < len(rs) && rs[k] == '.' && unicode.IsDigit(rs[k+1])) || d < 1 || d > 31 || m < 1 || m > 12 {
		return 0, false
	}
	return j + 1, true
}

func readDigits(rs []rune, i int) (string, int) {
	var b strings.Builder
	for i < len(rs) && unicode.IsDigit(rs[i]) {
		b.WriteRune(rs[i])
		i++
	}
	return b.String(), i
}

// Word lexicons. Left out on purpose: words that are common with another
// meaning ("once", "sept", "cent", "null", "secondo"), ordinals, and French
// 70-99. "one"/"uno"/"ein" are read but never required from a claim (weak).
var (
	numberWords  = map[string]float64{}
	wordLangs    = map[string]map[string]bool{} // number word -> languages it is a number in
	falseFriends = map[string]bool{"due": true, "sei": true, "seize": true, "dos": true, "otto": true, "elf": true,
		"mil": true, "un": true, "une": true, "una": true}
	multiplierWords = map[string]float64{
		"hundred": 100, "hundreds": 100, "cento": 100, "hundert": 100, "cien": 100, "ciento": 100,
		"thousand": 1e3, "thousands": 1e3, "mila": 1e3, "mille": 1e3, "tausend": 1e3, "mil": 1e3,
		"million": 1e6, "millions": 1e6, "milione": 1e6, "milioni": 1e6, "millionen": 1e6, "millón": 1e6, "millones": 1e6,
		"billion": 1e9, "billions": 1e9, "miliardo": 1e9, "miliardi": 1e9, "milliarde": 1e9, "milliarden": 1e9,
		"milliard": 1e9, "milliards": 1e9,
		"mln": 1e6, "mld": 1e9, "mrd": 1e9,
		"trillion": 1e12, "trillions": 1e12, "billionen": 1e12, "billón": 1e12, "billones": 1e12,
		"quadrillion": 1e15, "quadrillions": 1e15,
		"trillionen": 1e18, "trillón": 1e18, "trillones": 1e18,
	}
	// Italian papers often write the English trillion as "trilione", which by the book
	// is 10^18, and the same for "bilione".
	ambiguousScales = map[string][]float64{
		"bilione": {1e12, 1e9}, "bilioni": {1e12, 1e9},
		"trilione": {1e18, 1e12}, "trilioni": {1e18, 1e12},
	}
	scaleLinks  = map[string]bool{"di": true, "of": true, "de": true, "d": true, "von": true}
	hertzScales = map[string]float64{"khz": 1e3, "mhz": 1e6, "ghz": 1e9, "thz": 1e12}
	// vagueWords: the smallest value of a range in the plural and how many powers of ten it spans.
	vagueWords = map[string]struct {
		value float64
		span  int
	}{}
	decadeWords = map[string]int{
		"twenties": 20, "thirties": 30, "forties": 40, "fifties": 50, "sixties": 60, "seventies": 70, "eighties": 80, "nineties": 90,
		"zwanziger": 20, "dreißiger": 30, "vierziger": 40, "fünfziger": 50, "sechziger": 60, "siebziger": 70, "achtziger": 80,
		"neunziger": 90,
	}
	// month names and common abbreviations in the 5 languages: "30 giugno" is "30/06"
	monthWords   = map[string]int{}
	centuryWords = map[string]bool{
		"secolo": true, "secoli": true, "sec": true, "century": true, "centuries": true, "jahrhundert": true,
		"jahrhunderts": true, "siècle": true, "siècles": true, "siglo": true, "siglos": true,
	}
)

func init() {
	for _, g := range []struct {
		value float64
		span  int
		words string
	}{
		{1e1, 1, "decine tens dizaines decenas"},
		{1e2, 1, "centinaia hundreds hunderte centaines centenares cientos"},
		{1e3, 3, "migliaia thousands tausende milliers miles"},
		{1e4, 1, "zehntausende"},
		{1e5, 1, "hunderttausende"},
		{1e6, 3, "milioni millions millionen millones"},
		{1e9, 3, "miliardi billions milliarden milliards bilioni"},
		{1e12, 3, "trillions trilioni billionen billones"},
		{1e15, 3, "quadrillions"},
		{1e18, 3, "trillionen trillones"},
	} {
		for _, w := range strings.Fields(g.words) {
			vagueWords[w] = struct {
				value float64
				span  int
			}{g.value, g.span}
		}
	}
	lang := ""
	add := func(v float64, words ...string) {
		for _, w := range words {
			numberWords[w] = v
			if wordLangs[w] == nil {
				wordLangs[w] = map[string]bool{}
			}
			wordLangs[w][lang] = true
		}
	}
	months := map[string][]string{
		"en":    {"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"},
		"it":    {"gennaio", "febbraio", "marzo", "aprile", "maggio", "giugno", "luglio", "agosto", "settembre", "ottobre", "novembre", "dicembre"},
		"de":    {"januar", "februar", "märz", "april", "mai", "juni", "juli", "august", "september", "oktober", "november", "dezember"},
		"fr":    {"janvier", "février", "mars", "avril", "mai", "juin", "juillet", "août", "septembre", "octobre", "novembre", "décembre"},
		"es":    {"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"},
		"abbr":  {"jan", "feb", "mar", "apr", "", "jun", "jul", "aug", "sep", "oct", "nov", "dec"},
		"abbr2": {"gen", "", "", "", "mag", "giu", "lug", "ago", "set", "ott", "", "dic"},
		"abbr3": {"ene", "", "mär", "abr", "", "", "", "", "sept", "okt", "", "dez"},
	}
	for _, names := range months {
		for i, m := range names {
			if m != "" {
				monthWords[m] = i + 1
			}
		}
	}
	lang = "en"
	en := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve",
		"thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen"}
	for i, w := range en {
		add(float64(i), w)
	}
	for i, w := range []string{"twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"} {
		add(float64(20+10*i), w)
	}
	lang = "it"
	// Italian, with the joined forms (ventitré, trentotto, duemila, trecento)
	it := []string{"zero", "uno", "due", "tre", "quattro", "cinque", "sei", "sette", "otto", "nove", "dieci", "undici", "dodici",
		"tredici", "quattordici", "quindici", "sedici", "diciassette", "diciotto", "diciannove"}
	for i, w := range it {
		add(float64(i), w)
	}
	add(1, "una", "un")
	itTens := []string{"venti", "trenta", "quaranta", "cinquanta", "sessanta", "settanta", "ottanta", "novanta"}
	for i, t := range itTens {
		tv := float64(20 + 10*i)
		add(tv, t)
		for u := 1; u <= 9; u++ {
			stem := t
			if u == 1 || u == 8 {
				stem = t[:len(t)-1] // ventuno, ventotto
			}
			unit := it[u]
			add(tv+float64(u), stem+unit)
			if u == 3 {
				add(tv+3, stem+"tré")
			}
		}
	}
	for v := 2; v <= 9; v++ {
		add(float64(v*100), it[v]+"cento")
	}
	// "duemila", "trentaseimila": every Italian word from 2 to 99, joined with "mila"
	for w, v := range numberWords {
		if wordLangs[w]["it"] && v >= 2 && v <= 99 && v == math.Trunc(v) {
			add(v*1000, w+"mila")
		}
	}
	lang = "de"
	// German, with the joined forms (dreiundzwanzig, zweitausend)
	de := []string{"null", "eins", "zwei", "drei", "vier", "fünf", "sechs", "sieben", "acht", "neun", "zehn", "elf", "zwölf",
		"dreizehn", "vierzehn", "fünfzehn", "sechzehn", "siebzehn", "achtzehn", "neunzehn"}
	for i, w := range de[1:] {
		add(float64(i+1), w)
	}
	add(1, "ein", "eine")
	deUnits := []string{"", "ein", "zwei", "drei", "vier", "fünf", "sechs", "sieben", "acht", "neun"}
	for i, t := range []string{"zwanzig", "dreißig", "vierzig", "fünfzig", "sechzig", "siebzig", "achtzig", "neunzig"} {
		tv := float64(20 + 10*i)
		add(tv, t)
		for u := 1; u <= 9; u++ {
			add(tv+float64(u), deUnits[u]+"und"+t)
		}
	}
	for v := 1; v <= 9; v++ {
		add(float64(v*100), deUnits[v]+"hundert")
	}
	// "zweitausend", "sechsunddreißigtausend"
	for w, v := range numberWords {
		if wordLangs[w]["de"] && v >= 1 && v <= 99 && v == math.Trunc(v) && w != "eine" {
			add(v*1000, w+"tausend")
		}
	}
	lang = "fr"
	// French up to 69 ("vingt-trois" is read as two words)
	fr := []string{"zéro", "un", "deux", "trois", "quatre", "cinq", "six", "", "huit", "neuf", "dix", "onze", "douze", "treize",
		"quatorze", "quinze", "seize"}
	for i, w := range fr {
		if w != "" {
			add(float64(i), w)
		}
	}
	add(1, "une")
	for i, w := range []string{"vingt", "trente", "quarante", "cinquante", "soixante"} {
		add(float64(20+10*i), w)
	}
	lang = "es"
	// Spanish, with veintiuno..veintinueve
	es := []string{"cero", "uno", "dos", "tres", "cuatro", "cinco", "seis", "siete", "ocho", "nueve", "diez", "", "doce", "trece",
		"catorce", "quince", "dieciséis", "diecisiete", "dieciocho", "diecinueve"}
	for i, w := range es {
		if w != "" {
			add(float64(i), w)
		}
	}
	for i, w := range []string{"veinte", "treinta", "cuarenta", "cincuenta", "sesenta", "setenta", "ochenta", "noventa"} {
		add(float64(20+10*i), w)
	}
	for u, w := range []string{"", "veintiuno", "veintidós", "veintitrés", "veinticuatro", "veinticinco", "veintiséis",
		"veintisiete", "veintiocho", "veintinueve"} {
		if w != "" {
			add(float64(20+u), w)
		}
	}
	add(22, "veintidos")
	add(23, "veintitres")
	add(26, "veintiseis")
}
