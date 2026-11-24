package search

import (
	"math"
	"net/url"
	"sort"
	"strings"
)

type Intent struct {
	NormativeScore float64
	ProjectScore   float64
	QueryLevel     string // "national_region" | "municipal" | "none"
	RegionToponym  string // e.g. "sicilia", "campania", "emilia romagna", or "" if national/unspecified
	Ambiguous      bool
}

type IntentRerankerOptions struct {
	AuthorityBoost   float64
	ProjectPenalty   float64
	MunicipalPenalty float64
	ProjectBoost               float64
	Preset                     string   // Level 3 domain preset (e.g. "it")
	CustomAuthoritativeDomains []string // Domains nominated by LLM query ontology (Level 2)
}

func DefaultIntentRerankerOptions() IntentRerankerOptions {
	return IntentRerankerOptions{
		AuthorityBoost:   0.30,
		ProjectPenalty:   0.50,
		MunicipalPenalty: 0.80,
		ProjectBoost:     0.50,
	}
}

// nationalDomains contains generic public governance authorities (Level 1 core).
// Specialized vertical domain authorities (energy/storage/CEI) are modularized into Level 3 presets.
var nationalDomains = []string{
	"normattiva.it", "gazzettaufficiale.it", "vigilfuoco.it",
	"gse.it", "mase.gov.it", "mit.gov.it",
}

var regionalDomainPrefixes = []string{
	"regione.", "arpa.",
}

// singleWordNormative contains generic legal taxonomy markers.
// Benchmark-specific acronyms (pas, au, vvf, dcprev, pnrr, cei) are moved to Level 3 presets.
var singleWordNormative = map[string]bool{
	"normativa": true, "decreto": true, "legge": true, "circolare": true,
	"requisiti": true, "autorizzazione": true, "dm": true,
	"delibera": true, "disciplina": true, "regolamento": true, "direttiva": true,
}

var multiWordNormative = []string{
	"d.l.", "d.lgs.", "d.m.", "linee guida", "aree idonee", "vigili del fuoco",
}

var singleWordProject = map[string]bool{
	"progettista": true, "richiedente": true, "depositato": true,
	"determina": true, "cig": true, "cup": true,
}

var multiWordProject = []string{
	"relazione tecnica", "studio di fattibilità", "progettista incaricato",
	"ditta richiedente", "permesso di costruire", "particella catastale",
	"foglio di mappa", "rev. 0", "rev.0", "tavola n.", "codice identificativo",
	"s.r.l.", "s.p.a.", "comune di", "provincia di", "partita iva",
}

var regionalToponyms = map[string]bool{
	"italia": true, "sicilia": true, "puglia": true, "lazio": true, "lombardia": true,
	"veneto": true, "piemonte": true, "campania": true, "toscana": true, "calabria": true,
	"emilia": true, "liguria": true, "marche": true, "abruzzo": true, "umbria": true,
	"sardegna": true, "trentino": true, "friuli": true, "basilicata": true, "molise": true,
}

var multiWordRegionalToponyms = []string{
	"valle d'aosta", "val d'aosta", "emilia romagna", "friuli venezia giulia",
}

var municipalMarkers = []string{
	"comune di", "delibera comunale", "verbale di consiglio", "comitato di quartiere",
	"via comunale", "depositato presso il comune", "consiglio comunale", "città di",
}

// ClassifyIntentWithOptions analyzes a query and determines its normative vs project intent,
// accounting for any optional Level 3 domain preset.
func ClassifyIntentWithOptions(query string, opts IntentRerankerOptions) Intent {
	lowerQ := strings.ToLower(query)
	tokens := Tokenize(lowerQ)
	tokenSet := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		tokenSet[t] = true
	}

	var normScore float64
	for w := range singleWordNormative {
		if tokenSet[w] {
			normScore += 1.0
		}
	}
	// Level 3 preset keywords (e.g. Italian energy/storage benchmark acronyms)
	if IsPresetActive("it") || strings.EqualFold(opts.Preset, "it") {
		for w := range presetITKeywords {
			if tokenSet[w] {
				normScore += 1.0
			}
		}
	}
	for _, phrase := range multiWordNormative {
		if strings.Contains(lowerQ, phrase) {
			normScore += 1.0
		}
	}

	var projScore float64
	for w := range singleWordProject {
		if tokenSet[w] {
			projScore += 1.0
		}
	}
	for _, phrase := range multiWordProject {
		if strings.Contains(lowerQ, phrase) {
			projScore += 1.0
		}
	}

	queryLevel := "none"
	var regionToponym string
	normQ := normalizeToponym(lowerQ)

	// Check multi-word regional phrases first (e.g. "emilia romagna", "valle d'aosta")
	for _, regPhrase := range multiWordRegionalToponyms {
		if strings.Contains(normQ, normalizeToponym(regPhrase)) {
			queryLevel = "national_region"
			regionToponym = regPhrase
			break
		}
	}

	// Check single-word regional toponyms if no multi-word matched
	if regionToponym == "" {
		for _, reg := range tokens { // the first one written, the same at every run
			if regionalToponyms[reg] {
				queryLevel = "national_region"
				if reg != "italia" {
					regionToponym = reg
					break
				}
			}
		}
	} else if queryLevel == "none" {
		queryLevel = "national_region"
	}

	// Check for municipal markers in query
	for _, mun := range municipalMarkers {
		if strings.Contains(lowerQ, mun) {
			queryLevel = "municipal"
			break
		}
	}

	diff := math.Abs(normScore - projScore)
	ambiguous := (normScore == 0 && projScore == 0) || (diff < 1.0)

	return Intent{
		NormativeScore: normScore,
		ProjectScore:   projScore,
		QueryLevel:     queryLevel,
		RegionToponym:  regionToponym,
		Ambiguous:      ambiguous,
	}
}

// ClassifyIntent analyzes a query and determines its normative vs project intent.
func ClassifyIntent(query string) Intent {
	return ClassifyIntentWithOptions(query, IntentRerankerOptions{})
}

func normalizeToponym(s string) string {
	s = strings.ToLower(s)
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// hostIs reports whether a host is a domain or one of its own: "www.linkedin.de" is
// not "din.de".
func hostIs(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func isAuthoritativeFor(intent Intent, rawURL string, opts ...IntentRerankerOptions) bool {
	lowerURL := strings.ToLower(rawURL)
	u, err := url.Parse(lowerURL)
	var host string
	if err == nil && u.Host != "" {
		host = u.Host
	} else {
		host = lowerURL
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}

	preset := ""
	if len(opts) > 0 {
		preset = opts[0].Preset
	}

	// LLM query-nominated authoritative domains (Level 2)
	if len(opts) > 0 {
		for _, domain := range opts[0].CustomAuthoritativeDomains {
			d := strings.ToLower(strings.TrimSpace(domain))
			if d != "" && hostIs(host, d) {
				return true
			}
		}
	}

	// Universal international standards bodies (Level 1 core)
	for _, domain := range []string{"iso.org", "iec.ch", "din.de", "cen.eu", "cenelec.eu", "ieee.org", "ansi.org"} {
		if hostIs(host, domain) {
			return true
		}
	}

	// Domini nazionali generali della Repubblica (Livello 1 core)
	for _, domain := range nationalDomains {
		if hostIs(host, domain) {
			return true
		}
	}

	// Domini del preset "it" (Livello 3 opzionale)
	if IsPresetActive("it") || strings.EqualFold(preset, "it") {
		for _, domain := range presetITNationalDomains {
			if hostIs(host, domain) {
				return true
			}
		}
	}

	// Domini regionali (regione.*, arpa.*): autorevoli SOLO se la regione della query combacia
	isRegionalDomain := false
	for _, prefix := range regionalDomainPrefixes {
		if strings.Contains(host, prefix) {
			isRegionalDomain = true
			break
		}
	}

	if isRegionalDomain && intent.RegionToponym != "" {
		normHost := normalizeToponym(host)
		normRegion := normalizeToponym(intent.RegionToponym)
		if normRegion != "" && strings.Contains(normHost, normRegion) {
			return true
		}
	}

	return false
}

func isAuthoritative(rawURL string) bool {
	return isAuthoritativeFor(Intent{}, rawURL)
}

func hasProjectMarkers(r Result) bool {
	text := strings.ToLower(r.Title + " " + r.Content)
	tokens := Tokenize(text)
	tokenSet := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		tokenSet[t] = true
	}

	for w := range singleWordProject {
		if tokenSet[w] {
			return true
		}
	}
	for _, phrase := range multiWordProject {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func hasMunicipalMarkers(r Result) bool {
	text := strings.ToLower(r.Title + " " + r.Content)
	for _, marker := range municipalMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

var cvTitleMarkers = []string{
	// Italian
	"curriculum vitae", "curriculum-vitae", "cv di ", "formato europeo per il curriculum",
	// English
	"resume", "curriculum vitae", "academic cv", "curriculum-vitae",
	// German, French, Spanish
	"lebenslauf", "thèse de doctorat", "tesis doctoral",
}

var cvContentMarkers = []string{
	// Italian
	"laurea triennale", "laurea magistrale", "esami sostenuti", "dottore di ricerca in", "tesi di laurea",
	// English
	"bachelor of science", "bachelor of arts", "master of science", "master of arts",
	"ph.d. in", "doctor of philosophy in", "dissertation title", "academic transcript", "grade point average",
	// German, French, Spanish
	"abschlussarbeit", "doktorarbeit", "licenciatura en",
}

func isCVOrResumeDoc(r Result) bool {
	lowerTitle := strings.ToLower(r.Title)
	for _, m := range cvTitleMarkers {
		if strings.Contains(lowerTitle, m) {
			return true
		}
	}
	if strings.HasPrefix(lowerTitle, "cv ") || strings.HasSuffix(lowerTitle, " cv") {
		return true
	}

	lowerContent := strings.ToLower(r.Content)
	hasResumeToken := strings.Contains(lowerContent, "curriculum vitae") ||
		strings.Contains(lowerContent, "resume") ||
		strings.Contains(lowerContent, "lebenslauf")

	if hasResumeToken {
		for _, m := range cvContentMarkers {
			if strings.Contains(lowerContent, m) {
				return true
			}
		}
	}
	return false
}

// ScoreResult applies intent and domain authority adjustments to a base score.
func ScoreResult(intent Intent, r Result, baseScore float64, opts IntentRerankerOptions) float64 {
	isCustomAuth := false
	if len(opts.CustomAuthoritativeDomains) > 0 {
		lowerURL := strings.ToLower(r.URL)
		for _, cad := range opts.CustomAuthoritativeDomains {
			cad = strings.TrimSpace(cad)
			if cad != "" && strings.Contains(lowerURL, strings.ToLower(cad)) {
				isCustomAuth = true
				break
			}
		}
	}

	if intent.Ambiguous && !isCustomAuth {
		if isCVOrResumeDoc(r) {
			return baseScore - 0.60
		}
		return baseScore
	}

	s := baseScore
	auth := isAuthoritativeFor(intent, r.URL, opts)

	if (auth && intent.NormativeScore >= intent.ProjectScore) || isCustomAuth {
		s += opts.AuthorityBoost
	}

	// CV noise penalty: personal resumes/CVs when query is conceptual or research-driven
	if isCVOrResumeDoc(r) {
		s -= 0.60
	}

	// Authoritative domains are strictly IMMUNE to project and municipal penalties
	if !auth && !isCustomAuth {
		if intent.NormativeScore > intent.ProjectScore && hasProjectMarkers(r) {
			s -= opts.ProjectPenalty
		}
		if intent.QueryLevel == "national_region" && hasMunicipalMarkers(r) {
			s -= opts.MunicipalPenalty
		}
	}

	if intent.ProjectScore > intent.NormativeScore && hasProjectMarkers(r) {
		s += opts.ProjectBoost
	}

	return s
}

// IntentReranker wraps a base Reranker and modulates scores based on query intent and domain authority.
type IntentReranker struct {
	base Reranker
	opts IntentRerankerOptions
}

// WithCustomDomains returns a new IntentReranker copy augmented with custom authoritative domains.
func (r *IntentReranker) WithCustomDomains(domains []string) *IntentReranker {
	opts := r.opts
	opts.CustomAuthoritativeDomains = append([]string(nil), opts.CustomAuthoritativeDomains...)
	opts.CustomAuthoritativeDomains = append(opts.CustomAuthoritativeDomains, domains...)
	return NewIntentReranker(r.base, opts)
}

func (r *IntentReranker) GetOptions() IntentRerankerOptions {
	return r.opts
}

func (r *IntentReranker) SetCustomDomains(domains []string) {
	r.opts.CustomAuthoritativeDomains = append([]string(nil), domains...)
}

func NewIntentReranker(base Reranker, opts IntentRerankerOptions) *IntentReranker {
	return &IntentReranker{
		base: base,
		opts: opts,
	}
}

// Rerank satisfies the standard Reranker interface.
func (r *IntentReranker) Rerank(query string, results []Result, minScore float64) []Result {
	return r.RerankWithRoot(query, "", results, minScore)
}

// RerankWithRoot satisfies RootAwareReranker, allowing ambiguous sub-queries to inherit the root query's intent.
func (r *IntentReranker) RerankWithRoot(query string, rootQuery string, results []Result, minScore float64) []Result {
	if len(results) == 0 {
		return nil
	}
	if r.base == nil {
		return results
	}

	// Invoke base reranker with minScore=0.0 so authoritative domains with lower initial lexical scores are not discarded
	ranked := r.base.Rerank(query, results, 0.0)
	if len(ranked) == 0 {
		return nil
	}

	intent := ClassifyIntentWithOptions(query, r.opts)

	// If sub-query intent is ambiguous, inherit polarity and territorial scope from root user query
	if intent.Ambiguous && strings.TrimSpace(rootQuery) != "" {
		rootIntent := ClassifyIntentWithOptions(rootQuery, r.opts)
		if !rootIntent.Ambiguous {
			intent.NormativeScore = rootIntent.NormativeScore
			intent.ProjectScore = rootIntent.ProjectScore
			if intent.QueryLevel == "none" {
				intent.QueryLevel = rootIntent.QueryLevel
			}
			if intent.RegionToponym == "" {
				intent.RegionToponym = rootIntent.RegionToponym
			}
			intent.Ambiguous = false
		}
	}

	// Determine if base scores are already normalized (e.g. cosine similarities in [-1, 1])
	isAlreadyNormalized := true
	for _, item := range ranked {
		if item.Score < -1.0 || item.Score > 1.0 {
			isAlreadyNormalized = false
			break
		}
	}

	scored := make([]Result, len(ranked))
	copy(scored, ranked)

	for i := range scored {
		var normBase float64
		if isAlreadyNormalized {
			if scored[i].Score < 0 {
				normBase = 0.0
			} else {
				normBase = scored[i].Score
			}
		} else {
			// Soft saturation for BM25: s / (s + 5.0) smoothly maps [0, +inf) to [0, 1) without division by zero
			if scored[i].Score <= 0 {
				normBase = 0.0
			} else {
				normBase = scored[i].Score / (scored[i].Score + 5.0)
			}
		}

		scored[i].Score = ScoreResult(intent, scored[i], normBase, r.opts)
	}

	// Sort descending by modulated score
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	// Apply minScore threshold if requested, maintaining at least top-1 safeguard
	if minScore > 0 {
		var filtered []Result
		for _, s := range scored {
			if s.Score >= minScore {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) > 0 {
			return filtered
		}
		return scored[:1]
	}

	return scored
}
