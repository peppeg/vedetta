package search

import (
	"strings"
	"testing"
)

func TestClassifyIntent_NormativeAndTerritorial(t *testing.T) {
	intent := ClassifyIntent("agrivoltaico in Sicilia PAS AU normativa")
	if intent.Ambiguous {
		t.Errorf("expected unambiguous normative intent, got ambiguous=true")
	}
	if intent.NormativeScore <= intent.ProjectScore {
		t.Errorf("expected NormativeScore > ProjectScore, got norm=%f, proj=%f", intent.NormativeScore, intent.ProjectScore)
	}
	if intent.QueryLevel != "national_region" {
		t.Errorf("expected QueryLevel=national_region for Sicilia, got %q", intent.QueryLevel)
	}
}

func TestClassifyIntent_TechnicalAcronyms(t *testing.T) {
	// 1. In core agnostic mode (preset disabled): acronyms VVF and DCPREV are neutral
	ResetPresets()
	intentCore := ClassifyIntent("BESS litio > 1 MWh thermal runaway VVF DCPREV")
	if intentCore.NormativeScore > 0 {
		t.Errorf("expected 0 normative score in core agnostic mode for VVF/DCPREV, got %f", intentCore.NormativeScore)
	}

	// 2. With Level 3 Preset "it" enabled: acronyms VVF and DCPREV are recognized
	SetPresetActive("it", true)
	defer ResetPresets()

	intentPreset := ClassifyIntent("BESS litio > 1 MWh thermal runaway VVF DCPREV")
	if intentPreset.Ambiguous {
		t.Errorf("expected technical acronyms VVF and DCPREV to classify as normative under preset IT, got ambiguous")
	}
	if intentPreset.NormativeScore < 2.0 {
		t.Errorf("expected at least 2.0 for VVF and DCPREV under preset IT, got %f", intentPreset.NormativeScore)
	}
}

func TestClassifyIntent_ProjectMunicipal(t *testing.T) {
	intent := ClassifyIntent("autorizzazione impianto BESS del Comune di Carpi relazione tecnica")
	if intent.ProjectScore <= intent.NormativeScore {
		t.Errorf("expected ProjectScore > NormativeScore, got norm=%f, proj=%f", intent.NormativeScore, intent.ProjectScore)
	}
	if intent.QueryLevel != "municipal" {
		t.Errorf("expected QueryLevel=municipal, got %q", intent.QueryLevel)
	}
}

func TestScoreResult_CarpiDemotionAndAuthorityImmunity(t *testing.T) {
	opts := DefaultIntentRerankerOptions()
	intent := ClassifyIntent("agrivoltaico in Sicilia PAS AU normativa")

	carpiDoc := Result{
		Title:   "Delibera del Comune di Carpi su impianto BESS Fossoli Nord",
		URL:     "https://comune.carpi.mo.it/atti/delibera123",
		Content: "Relazione tecnica del progettista incaricato depositato presso il comune di Carpi.",
	}

	normattivaDoc := Result{
		Title:   "D.Lgs. 199/2021 testo coordinato",
		URL:     "https://www.normattiva.it/uri-res/N2Ls?urn:nir:stato:decreto.legislativo:2021-11-08;199",
		Content: "Attuazione della direttiva UE sulla promozione dell'uso dell'energia da fonti rinnovabili, d'intesa con il comune di competenza.",
	}

	baseScore := 0.50

	carpiScore := ScoreResult(intent, carpiDoc, baseScore, opts)
	// Carpi doc should be hit by both projectPenalty (-0.50) and municipalPenalty (-0.80)
	if carpiScore >= baseScore {
		t.Errorf("expected Carpi doc score to be heavily penalized, got %f (base=%f)", carpiScore, baseScore)
	}

	normattivaScore := ScoreResult(intent, normattivaDoc, baseScore, opts)
	// Normattiva doc is authoritative, must get authorityBoost (+0.30) and be immune to municipal penalty
	expectedScore := baseScore + opts.AuthorityBoost
	if normattivaScore != expectedScore {
		t.Errorf("expected normattiva score %f, got %f", expectedScore, normattivaScore)
	}
}

func TestIntentReranker_RerankFlow(t *testing.T) {
	baseReranker := NewBM25Reranker()
	intentReranker := NewIntentReranker(baseReranker, DefaultIntentRerankerOptions())

	results := []Result{
		{
			Title:   "Comune di Carpi relazione tecnica",
			URL:     "https://comune.carpi.mo.it/delibera",
			Content: "Studio di fattibilità depositato presso il comune di Carpi per impianto.",
		},
		{
			Title:   "Norme agrivoltaico e PAS",
			URL:     "https://www.normattiva.it/eli/id/2021/11/30/21G00214/CONSOLIDATED",
			Content: "Disciplina dell'agrivoltaico avanzato e regimi di autorizzazione PAS e AU.",
		},
	}

	query := "agrivoltaico avanzato in Sicilia con BESS al litio: PAS vs AU normativa"
	ranked := intentReranker.Rerank(query, results, 0.05)

	if len(ranked) == 0 {
		t.Fatalf("expected results from rerank, got none")
	}

	if !strings.Contains(ranked[0].URL, "normattiva.it") {
		t.Errorf("expected authoritative normattiva.it to be ranked first, got %s (score=%f) ahead of %s (score=%f)",
			ranked[0].URL, ranked[0].Score, ranked[1].URL, ranked[1].Score)
	}
}

func TestIntentReranker_RootQueryInheritance(t *testing.T) {
	baseReranker := NewBM25Reranker()
	intentReranker := NewIntentReranker(baseReranker, DefaultIntentRerankerOptions())

	results := []Result{
		{
			Title:   "Studio accumulo batterie litio",
			URL:     "https://generic-blog.example.com/batterie",
			Content: "Studio tecnico generico sui sistemi di accumulo stazionario con batterie al litio.",
		},
		{
			Title:   "Vigili del Fuoco sicurezza BESS",
			URL:     "https://www.vigilfuoco.it/lineeguida",
			Content: "Sicurezza antincendio e sistemi di accumulo stazionario con batterie al litio.",
		},
	}

	// Ambiguous sub-query (no explicit norm or project markers)
	subQuery := "sistemi di accumulo batterie litio"
	rootQuery := "disciplina autorizzativa e sicurezza BESS in Sicilia normativa"

	// Without root query: subQuery is ambiguous, neither gets boosted or penalized
	rankedWithoutRoot := intentReranker.Rerank(subQuery, results, 0.0)
	// With root query: subQuery inherits normative polarity from rootQuery -> vigilfuoco.it gets authorityBoost
	rankedWithRoot := intentReranker.RerankWithRoot(subQuery, rootQuery, results, 0.0)

	var scoreWithoutAuth, scoreWithAuth float64
	for _, r := range rankedWithoutRoot {
		if strings.Contains(r.URL, "vigilfuoco.it") {
			scoreWithoutAuth = r.Score
		}
	}
	for _, r := range rankedWithRoot {
		if strings.Contains(r.URL, "vigilfuoco.it") {
			scoreWithAuth = r.Score
		}
	}

	if scoreWithAuth <= scoreWithoutAuth {
		t.Errorf("expected vigilfuoco.it to receive authorityBoost when root query intent is inherited: with=%f, without=%f",
			scoreWithAuth, scoreWithoutAuth)
	}
}

func TestTerritorialAlignment_RegionAwareAuthority(t *testing.T) {
	opts := DefaultIntentRerankerOptions()

	// 1. Query for Sicilia
	querySicilia := "agrivoltaico in Sicilia PAS AU normativa"
	intentSicilia := ClassifyIntent(querySicilia)
	if intentSicilia.RegionToponym != "sicilia" {
		t.Fatalf("expected RegionToponym=sicilia, got %q", intentSicilia.RegionToponym)
	}

	siciliaDoc := Result{
		Title:   "Regione Siciliana - Assessorato Energia - Linee Guida Agrivoltaico",
		URL:     "https://www.regione.sicilia.it/energia/agrivoltaico/lineeguida",
		Content: "Decreto assessoriale recante disciplina attuativa per impianti agrivoltaici in Sicilia.",
	}
	campaniaBurcDoc := Result{
		Title:   "BURC Regione Campania - Progetto Agro-FV Castelvolturno",
		URL:     "https://burc.regione.campania.it/atti/progetto123",
		Content: "Relazione tecnica del progettista incaricato depositato presso il comune di Castelvolturno con particella catastale e foglio di mappa.",
	}
	normattivaDoc := Result{
		Title:   "Normattiva - D.Lgs. 199/2021",
		URL:     "https://www.normattiva.it/eli/id/2021/11/30/21G00214/CONSOLIDATED",
		Content: "Disciplina nazionale rinnovabili e aree idonee.",
	}

	baseScore := 0.50

	// regione.sicilia.it should get AuthorityBoost (+0.30)
	scoreSicilia := ScoreResult(intentSicilia, siciliaDoc, baseScore, opts)
	expectedSiciliaScore := baseScore + opts.AuthorityBoost
	if scoreSicilia != expectedSiciliaScore {
		t.Errorf("expected regione.sicilia.it to get AuthorityBoost %f, got %f", expectedSiciliaScore, scoreSicilia)
	}

	// burc.regione.campania.it is for a different region -> must NOT get AuthorityBoost,
	// and because !auth it must be penalized by ProjectPenalty and MunicipalPenalty
	scoreCampania := ScoreResult(intentSicilia, campaniaBurcDoc, baseScore, opts)
	if scoreCampania >= baseScore {
		t.Errorf("expected burc.regione.campania.it to be heavily penalized for Sicilia query, got %f (base=%f)",
			scoreCampania, baseScore)
	}

	// normattiva.it is national -> always authoritative, gets AuthorityBoost
	scoreNormattiva := ScoreResult(intentSicilia, normattivaDoc, baseScore, opts)
	if scoreNormattiva != expectedSiciliaScore {
		t.Errorf("expected normattiva.it to get AuthorityBoost %f, got %f", expectedSiciliaScore, scoreNormattiva)
	}

	// 2. National query without region
	queryNational := "disciplina autorizzativa BESS litio normativa CEI"
	intentNational := ClassifyIntent(queryNational)
	if intentNational.RegionToponym != "" {
		t.Errorf("expected empty RegionToponym for national query, got %q", intentNational.RegionToponym)
	}

	// On national query, regional domain must NOT be authoritative
	campaniaNormativeDoc := Result{
		Title:   "Regione Campania - Circolare BESS",
		URL:     "https://regione.campania.it/energia/bess",
		Content: "Circolare su accumuli elettrochimici.",
	}
	scoreCampaniaNat := ScoreResult(intentNational, campaniaNormativeDoc, baseScore, opts)
	if scoreCampaniaNat > baseScore {
		t.Errorf("expected regional domain to not get AuthorityBoost on national query, got %f", scoreCampaniaNat)
	}

	// 3. Multi-word region (Emilia-Romagna)
	queryER := "agrivoltaico in Emilia-Romagna PAS AU disciplina"
	intentER := ClassifyIntent(queryER)
	if intentER.RegionToponym != "emilia romagna" {
		t.Errorf("expected RegionToponym='emilia romagna', got %q", intentER.RegionToponym)
	}

	erDoc := Result{
		Title:   "Regione Emilia-Romagna - Linee Guida Agrivoltaico",
		URL:     "https://demetra.regione.emilia-romagna.it/al/articolo?id=123",
		Content: "Delibera di Giunta regionale Emilia-Romagna.",
	}
	scoreER := ScoreResult(intentER, erDoc, baseScore, opts)
	if scoreER != expectedSiciliaScore {
		t.Errorf("expected regione.emilia-romagna.it to get AuthorityBoost %f, got %f", expectedSiciliaScore, scoreER)
	}
}

func TestTerritorialAlignment_RootQueryInheritance(t *testing.T) {
	baseReranker := NewBM25Reranker()
	intentReranker := NewIntentReranker(baseReranker, DefaultIntentRerankerOptions())

	results := []Result{
		{
			Title:   "Regione Siciliana disciplina BESS",
			URL:     "https://www.regione.sicilia.it/energia/bess",
			Content: "Disposizioni regionali per impianti di accumulo e batterie in Sicilia.",
		},
		{
			Title:   "BURC Regione Campania BESS Castelvolturno",
			URL:     "https://burc.regione.campania.it/progetto-bess",
			Content: "Relazione tecnica del progettista depositato comune di Castelvolturno particella catastale.",
		},
	}

	subQuery := "sistemi di accumulo batterie litio" // Ambiguous
	rootQuery := "disciplina autorizzativa e sicurezza BESS in Sicilia normativa"

	rankedWithRoot := intentReranker.RerankWithRoot(subQuery, rootQuery, results, 0.0)
	if len(rankedWithRoot) < 2 {
		t.Fatalf("expected 2 ranked results, got %d", len(rankedWithRoot))
	}

	// regione.sicilia.it should inherit the authority boost from root query's Sicilia toponym
	if !strings.Contains(rankedWithRoot[0].URL, "regione.sicilia.it") {
		t.Errorf("expected regione.sicilia.it to rank first after inheriting root query region, got %s",
			rankedWithRoot[0].URL)
	}
	if rankedWithRoot[0].Score <= rankedWithRoot[1].Score {
		t.Errorf("expected sicilia.it score (%f) > campania score (%f)",
			rankedWithRoot[0].Score, rankedWithRoot[1].Score)
	}
}

func TestScoreResult_CEIDomainsAuthority(t *testing.T) {
	ResetPresets()
	defer ResetPresets()

	ceiURLs := []string{
		"https://mycatalogo.ceinorme.it/norma/cei-0-16",
		"https://www.ceiweb.it/pubblicazioni/standard-cei-0-16",
		"https://static.ceinorme.it/allegati/norma_cei_016.pdf",
		"https://www.iec.ch/dyn/www/f?p=103:22:0::::FSP_ORG_ID:1234",
	}

	doc := Result{
		Title:   "Standard CEI 0-16",
		URL:     ceiURLs[0],
		Content: "Regola tecnica di riferimento per la connessione di utenti attivi e passivi.",
	}

	// 1. Core agnostic mode (preset disabled): CEI URL is neutral
	optsCore := DefaultIntentRerankerOptions()
	intentCore := ClassifyIntent("norma CEI 0-16 connessione alla rete MT BT requisiti tecnici")
	scoreCore := ScoreResult(intentCore, doc, 0.50, optsCore)
	if scoreCore != 0.50 {
		t.Errorf("expected neutral score (0.50) in core agnostic mode for CEI domain, got %f", scoreCore)
	}

	// 2. Preset IT mode: CEI URLs receive AuthorityBoost
	optsPreset := DefaultIntentRerankerOptions()
	optsPreset.Preset = "it"
	intentPreset := ClassifyIntentWithOptions("norma CEI 0-16 connessione alla rete MT BT requisiti tecnici", optsPreset)

	for _, u := range ceiURLs {
		ceiDoc := Result{
			Title:   "Standard CEI 0-16",
			URL:     u,
			Content: "Regola tecnica di riferimento per la connessione di utenti attivi e passivi.",
		}
		score := ScoreResult(intentPreset, ceiDoc, 0.50, optsPreset)
		expected := 0.50 + optsPreset.AuthorityBoost
		if score < expected-0.001 {
			t.Errorf("expected CEI URL %s to receive AuthorityBoost under preset IT (score >= %f), got %f", u, expected, score)
		}
	}
}

func TestScoreResult_CustomAuthoritativeDomains(t *testing.T) {
	opts := DefaultIntentRerankerOptions()
	opts.CustomAuthoritativeDomains = []string{"plato.stanford.edu", "ncbi.nlm.nih.gov"}

	intent := ClassifyIntent("concetto filosofico speranza Ernst Bloch")

	docSEP := Result{
		Title:   "Hope (Stanford Encyclopedia of Philosophy)",
		URL:     "https://plato.stanford.edu/entries/hope/",
		Content: "Philosophical analysis of the concept of hope from ancient thought to contemporary epistemology.",
	}

	scoreSEP := ScoreResult(intent, docSEP, 0.50, opts)
	expectedSEP := 0.50 + opts.AuthorityBoost
	if scoreSEP < expectedSEP-0.001 {
		t.Errorf("expected custom domain plato.stanford.edu to receive AuthorityBoost (%f), got %f", expectedSEP, scoreSEP)
	}

	docBlog := Result{
		Title:   "Pensieri sparsi sulla speranza",
		URL:     "https://random-blog.com/speranza",
		Content: "Riflessione personale sulla speranza.",
	}
	scoreBlog := ScoreResult(intent, docBlog, 0.50, opts)
	if scoreBlog != 0.50 {
		t.Errorf("expected neutral blog to have score 0.50, got %f", scoreBlog)
	}
}

func TestScoreResult_CVDemotion(t *testing.T) {
	opts := DefaultIntentRerankerOptions()
	intent := ClassifyIntent("La speranza concetto filosofico implicazioni psicologiche")

	docCV := Result{
		Title:   "Curriculum Vitae - Dott.ssa Marianna Liotti",
		URL:     "https://www.unipa.it/docenti/liotti/cv.pdf",
		Content: "Curriculum Vitae et studiorum. Laurea triennale e magistrale in discipline umanistiche con tesi finale.",
	}

	scoreCV := ScoreResult(intent, docCV, 0.50, opts)
	if scoreCV >= 0.0 {
		t.Errorf("expected CV document to be heavily demoted (score < 0), got %f", scoreCV)
	}
}
