package agent

// Provenance is a deterministic, user-facing statement about how a response was grounded.
// It is computed in Go (never by the LLM) so it cannot be "argued away" by the model.
type Provenance string

const (
	// ProvenanceGrounded: the answer is based on verified sources/facts.
	ProvenanceGrounded Provenance = "grounded"
	// ProvenancePartial: the answer is incomplete (synthesis cut by a limit/loop).
	ProvenancePartial Provenance = "partial"
	// ProvenanceParametric: the answer cites no verified fact (none was produced, or
	// the writer used none of them); it rests on previews and model knowledge.
	ProvenanceParametric Provenance = "parametric"
	// ProvenanceDegraded: the pipeline was degraded (incompatible model / failed stages).
	ProvenanceDegraded Provenance = "degraded"
	// ProvenanceAnchored: the answer reuses verified evidence from the same session (no new search).
	ProvenanceAnchored Provenance = "anchored"
	// ProvenanceResearched: the Deep a ciclo cites pages the model read. Go checks its
	// links and numbers, not every sentence against the page it links, so it is never
	// called verified (audit of 05/11/2026, docs/V25_LOG_VALUTAZIONI.md).
	ProvenanceResearched Provenance = "researched"
)

// ComputeProvenance derives the session provenance from observable signals:
// "grounded" only when the answer cites at least one verified fact (V25-QA A3).
// Precedence: degraded > parametric (no fact cited) > partial (incomplete) > grounded.
func ComputeProvenance(factsCited int, partial, degraded bool) Provenance {
	switch {
	case degraded:
		return ProvenanceDegraded
	case factsCited == 0:
		return ProvenanceParametric
	case partial:
		return ProvenancePartial
	default:
		return ProvenanceGrounded
	}
}

// loopBasis is what the pages the model read give to an answer of the Deep a ciclo. An
// answer with none of it rests on previews and on the model's knowledge, also when Vedetta
// opened some of the pages it links: that is a check, not a reading.
type loopBasis struct {
	cited  int // pages read, in this turn or before, that the answer links
	quotes int // sentences whose quote was found on a page read
}

func (s LoopStats) basis() loopBasis {
	b := loopBasis{cited: s.PagesCited + s.PagesCitedBefore}
	if s.Quotes == "done" {
		for _, it := range s.QuoteItems {
			if it.State == quoteFound {
				b.quotes++
			}
		}
	}
	return b
}

// loopProvenance is ComputeProvenance for the Deep a ciclo, where nothing is a
// verified fact: pages behind the answer make it researched, not grounded.
func loopProvenance(b loopBasis, partial, degraded bool) Provenance {
	if p := ComputeProvenance(b.cited+b.quotes, partial, degraded); p != ProvenanceGrounded {
		return p
	}
	return ProvenanceResearched
}

// basisNote completes the line of a researched answer that links no page the model read:
// the pages are there, but not where the reader would look for them.
func basisNote(b loopBasis, lang string) string {
	if b.cited > 0 || b.quotes == 0 {
		return ""
	}
	return " " + StepMessage(lang, "loop_basis_quotes", StepMessage(lang, "loop_quotes_list_title"))
}

// loopProvenanceNote is provenanceNote for the Deep a ciclo. After a research, an
// answer that cites no page read rests on previews, not on the model's knowledge alone.
// With the check of the cited pages, the note says what is checked and no more; both
// researched notes say that judgments, advice and comparisons are the model's (map R5).
func loopProvenanceNote(p Provenance, lang string, researched, checked bool) string {
	if p == ProvenanceResearched && checked {
		switch NormalizeLanguage(lang) {
		case "en":
			return "\n\n> **Provenance: researched** — research led by the model: Vedetta checks links, numbers and the cited pages the model did not read, not every sentence: judgments, advice and comparisons are the model's."
		case "de":
			return "\n\n> **Herkunft: researched** — vom Modell geführte Recherche: Vedetta prüft Links, Zahlen und die zitierten Seiten, die das Modell nicht gelesen hat, nicht jeden Satz: Urteile, Ratschläge und Vergleiche stammen vom Modell."
		case "fr":
			return "\n\n> **Provenance : researched** — recherche guidée par le modèle : Vedetta contrôle les liens, les nombres et les pages citées que le modèle n'a pas lues, pas chaque phrase : les jugements, conseils et comparaisons sont ceux du modèle."
		case "es":
			return "\n\n> **Procedencia: researched** — investigación guiada por el modelo: Vedetta comprueba enlaces, números y las páginas citadas que el modelo no leyó, no cada frase: los juicios, consejos y comparaciones son del modelo."
		default:
			return "\n\n> **Provenienza: researched** — ricerca guidata dal modello: Vedetta controlla link, numeri e le pagine citate che il modello non ha letto, non ogni frase: giudizi, consigli e confronti sono del modello."
		}
	}
	if p != ProvenanceParametric || !researched {
		return provenanceNote(p, lang)
	}
	switch NormalizeLanguage(lang) {
	case "en":
		return "\n\n> **Provenance: parametric** — the answer cites no page it read: it rests on search previews and the model's knowledge."
	case "de":
		return "\n\n> **Herkunft: parametric** — die Antwort zitiert keine gelesene Seite: sie stützt sich auf Suchvorschauen und das Modellwissen."
	case "fr":
		return "\n\n> **Provenance : parametric** — la réponse ne cite aucune page lue : elle repose sur les aperçus de recherche et les connaissances du modèle."
	case "es":
		return "\n\n> **Procedencia: parametric** — la respuesta no cita ninguna página leída: se basa en las vistas previas de búsqueda y en el conocimiento del modelo."
	default:
		return "\n\n> **Provenienza: parametric** — la risposta non cita nessuna pagina letta: si basa sulle anteprime di ricerca e sulla conoscenza del modello."
	}
}

// provenanceNote renders a deterministic, localized one-line provenance statement
// meant to be appended to the answer (so it survives reloads and cannot be omitted).
func provenanceNote(p Provenance, lang string) string {
	l := NormalizeLanguage(lang)
	switch p {
	case ProvenanceGrounded:
		switch l {
		case "en":
			return "\n\n> **Provenance: grounded** — based on verified sources."
		case "de":
			return "\n\n> **Herkunft: grounded** — basierend auf verifizierten Quellen."
		case "fr":
			return "\n\n> **Provenance : grounded** — fondée sur des sources vérifiées."
		case "es":
			return "\n\n> **Procedencia: grounded** — basada en fuentes verificadas."
		default:
			return "\n\n> **Provenienza: grounded** — basata su fonti verificate."
		}
	case ProvenanceResearched:
		switch l {
		case "en":
			return "\n\n> **Provenance: researched** — research led by the model: Vedetta checks links and numbers, not every sentence: judgments, advice and comparisons are the model's."
		case "de":
			return "\n\n> **Herkunft: researched** — vom Modell geführte Recherche: Vedetta prüft Links und Zahlen, nicht jeden Satz: Urteile, Ratschläge und Vergleiche stammen vom Modell."
		case "fr":
			return "\n\n> **Provenance : researched** — recherche guidée par le modèle : Vedetta contrôle les liens et les nombres, pas chaque phrase : les jugements, conseils et comparaisons sont ceux du modèle."
		case "es":
			return "\n\n> **Procedencia: researched** — investigación guiada por el modelo: Vedetta comprueba enlaces y números, no cada frase: los juicios, consejos y comparaciones son del modelo."
		default:
			return "\n\n> **Provenienza: researched** — ricerca guidata dal modello: Vedetta controlla link e numeri, non ogni frase: giudizi, consigli e confronti sono del modello."
		}
	case ProvenancePartial:
		switch l {
		case "en":
			return "\n\n> **Provenance: partial** — the answer is incomplete (limit or interruption reached)."
		case "de":
			return "\n\n> **Herkunft: partial** — die Antwort ist unvollständig (Limit oder Unterbrechung erreicht)."
		case "fr":
			return "\n\n> **Provenance : partial** — la réponse est incomplète (limite ou interruption atteinte)."
		case "es":
			return "\n\n> **Procedencia: partial** — la respuesta está incompleta (se alcanzó un límite o interrupción)."
		default:
			return "\n\n> **Provenienza: partial** — la risposta è incompleta (limite o interruzione raggiunti)."
		}
	case ProvenanceParametric:
		switch l {
		case "en":
			return "\n\n> **Provenance: parametric** — no verified facts were found: this answer is based on the model's knowledge."
		case "de":
			return "\n\n> **Herkunft: parametric** — keine verifizierten Fakten gefunden: die Antwort basiert auf dem Modellwissen."
		case "fr":
			return "\n\n> **Provenance : parametric** — aucun fait vérifié trouvé : cette réponse est fondée sur les connaissances du modèle."
		case "es":
			return "\n\n> **Procedencia: parametric** — no se encontraron hechos verificados: esta respuesta se basa en el conocimiento del modelo."
		default:
			return "\n\n> **Provenienza: parametric** — nessun fatto verificato trovato: questa risposta si basa sulla conoscenza del modello."
		}
	case ProvenanceAnchored:
		switch l {
		case "en":
			return "\n\n> **Provenance: anchored** — based on the verified evidence already collected in this session (no new web search)."
		case "de":
			return "\n\n> **Herkunft: anchored** — basierend auf den bereits in dieser Sitzung gesammelten verifizierten Belegen (keine neue Websuche)."
		case "fr":
			return "\n\n> **Provenance : anchored** — fondée sur les preuves vérifiées déjà collectées dans cette session (aucune nouvelle recherche web)."
		case "es":
			return "\n\n> **Procedencia: anchored** — basada en la evidencia verificada ya recogida en esta sesión (sin nueva búsqueda web)."
		default:
			return "\n\n> **Provenienza: anchored** — basata sull'evidenza verificata già raccolta in questa sessione (nessuna nuova ricerca web)."
		}
	default: // ProvenanceDegraded
		switch l {
		case "en":
			return "\n\n> **Provenance: degraded** — the pipeline was degraded (model or tools); treat the answer with caution."
		case "de":
			return "\n\n> **Herkunft: degraded** — die Pipeline war beeinträchtigt (Modell oder Werkzeuge); Antwort mit Vorsicht behandeln."
		case "fr":
			return "\n\n> **Provenance : degraded** — la pipeline a été dégradée (modèle ou outils) ; à traiter avec prudence."
		case "es":
			return "\n\n> **Procedencia: degraded** — la pipeline se degradó (modelo o herramientas); tratar la respuesta con cautela."
		default:
			return "\n\n> **Provenienza: degraded** — pipeline degradata (modello o strumenti); trattare la risposta con cautela."
		}
	}
}
