package agent

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
)

// normalizeLangAlias recognizes language names and codes and returns the supported
// short code. The bool is false when the input is empty or unrecognized.
func normalizeLangAlias(lang string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "it", "italian", "italiano":
		return "it", true
	case "de", "german", "deutsch":
		return "de", true
	case "fr", "french", "français", "francais":
		return "fr", true
	case "es", "spanish", "español", "espanol":
		return "es", true
	case "en", "english", "inglese":
		return "en", true
	}
	return "", false
}

// NormalizeLanguage standardizes input language codes into supported locales: "it", "en", "de", "fr", "es".
// Defaults to "en" (English-first) if empty; unrecognized non-empty strings also map to "en".
func NormalizeLanguage(lang string) string {
	if code, ok := normalizeLangAlias(lang); ok {
		return code
	}
	return "en"
}

// --- Answer-language detection & resolution (v2.4: UI locale vs answer language) ---

var supportedLangs = []string{"it", "en", "de", "fr", "es"}

var langWordLists = map[string][]string{
	"it": {"il", "lo", "la", "i", "gli", "le", "un", "una", "uno", "di", "del", "dello", "della", "dei", "degli", "delle", "che", "chi", "cui", "per", "con", "non", "sono", "è", "e", "come", "cosa", "quale", "quali", "quando", "dove", "perché", "perche", "più", "piu", "anche", "molto", "poco", "senza", "tra", "fra", "nel", "nello", "nella", "nei", "negli", "nelle", "sul", "sullo", "sulla", "sui", "sugli", "sulle", "questo", "questa", "questi", "queste", "quello", "quella", "essere", "avere", "fare", "andare", "vedere", "vorrei", "trascorrere", "giorni", "giorno", "figlia", "figlio", "bambini", "bambino", "posti", "posto", "visitare", "conosciuto", "turistico", "valerne", "pena", "speranza", "evidenze", "cliniche", "normativa", "impianti", "grazie", "città", "citta", "famiglia", "viaggio", "migliore"},
	"en": {"the", "of", "to", "and", "is", "are", "was", "were", "be", "been", "for", "with", "that", "this", "these", "those", "what", "how", "why", "when", "where", "which", "who", "from", "have", "has", "had", "can", "could", "should", "would", "will", "not", "but", "you", "your", "about", "into", "than", "then", "there", "their", "hope", "clinical", "evidence", "psychology", "work", "works", "day", "days", "family", "place", "places", "visit", "child", "children", "research", "study", "studies", "health", "system", "guide", "trip", "best", "must", "see", "hidden", "gems", "local", "significant", "related", "between", "through", "during", "against"},
	"de": {"der", "die", "das", "den", "dem", "des", "und", "ist", "sind", "war", "waren", "nicht", "mit", "für", "fuer", "wie", "was", "warum", "wann", "wo", "wer", "wen", "wem", "ein", "eine", "einen", "einem", "einer", "eines", "ich", "du", "sie", "wir", "ihr", "auch", "aber", "oder", "kann", "können", "koennen", "muss", "müssen", "muessen", "werden", "wird", "zwischen", "über", "ueber", "durch", "nach", "bei", "aus", "noch", "nur", "schon", "funktioniert", "schulsystem", "hoffnung", "klinisch", "studie", "forschung", "familie", "reise"},
	"fr": {"le", "la", "les", "un", "une", "des", "du", "de", "et", "est", "sont", "était", "etait", "pour", "avec", "que", "qui", "quoi", "comment", "pourquoi", "quand", "où", "ou", "dans", "sur", "sous", "pas", "plus", "vous", "nous", "je", "tu", "il", "elle", "ils", "elles", "être", "etre", "avoir", "aux", "par", "ce", "cette", "ces", "mon", "ma", "mes", "fonctionne", "impôt", "impot", "revenu", "espoir", "clinique", "étude", "etude", "recherche", "famille", "voyage", "meilleur"},
	"es": {"el", "la", "los", "las", "un", "una", "unos", "unas", "de", "del", "es", "son", "era", "eran", "para", "con", "que", "qué", "como", "cómo", "por", "cuándo", "cuando", "dónde", "donde", "más", "mas", "pero", "sin", "entre", "desde", "hasta", "este", "esta", "estos", "estas", "muy", "también", "tambien", "nosotros", "ellos", "usted", "y", "o", "en", "comer", "esperanza", "clínica", "clinica", "evidencia", "familia", "niños", "ninos", "viaje", "días", "dias", "lugares", "ocultos", "mejor"},
}

var langLexicon = func() map[string]map[string]bool {
	m := make(map[string]map[string]bool, len(langWordLists))
	for lang, words := range langWordLists {
		set := make(map[string]bool, len(words))
		for _, w := range words {
			set[w] = true
		}
		m[lang] = set
	}
	return m
}()

// langDiacriticBonus adds language-diagnostic weight from characters and punctuation.
func langDiacriticBonus(lower, lang string) int {
	switch lang {
	case "es":
		b := 0
		if strings.ContainsAny(lower, "áíóúñ") {
			b += 2
		}
		if strings.ContainsAny(lower, "¿¡") {
			b += 3
		}
		return b
	case "de":
		b := 0
		if strings.Contains(lower, "ß") {
			b += 3
		}
		if strings.ContainsAny(lower, "äöü") {
			b += 2
		}
		return b
	case "fr":
		b := 0
		if strings.ContainsAny(lower, "çêëîïôû") {
			b += 2
		}
		if strings.ContainsAny(lower, "àéèù") {
			b += 1
		}
		return b
	case "it":
		b := 0
		if strings.ContainsAny(lower, "ì") {
			b += 2
		}
		if strings.ContainsAny(lower, "àèéòù") {
			b += 1
		}
		return b
	}
	return 0
}

// tokenizeLang lowercases and splits text into letter-only tokens (accents kept).
func tokenizeLang(text string) []string {
	var tokens []string
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) {
			b.WriteRune(r)
			continue
		}
		if b.Len() > 0 {
			tokens = append(tokens, b.String())
			b.Reset()
		}
	}
	if b.Len() > 0 {
		tokens = append(tokens, b.String())
	}
	return tokens
}

// DetectTextLanguage returns the language of the query text among the supported
// locales {it,en,de,fr,es}, or "" when it cannot decide reliably. Pure Go, no LLM.
func DetectTextLanguage(text string) string {
	tokens := tokenizeLang(text)
	if len(tokens) < 2 {
		return "" // too short to decide
	}
	score := make(map[string]int, len(supportedLangs))
	for _, t := range tokens {
		for _, lang := range supportedLangs {
			if langLexicon[lang][t] {
				score[lang]++
			}
		}
	}
	lower := strings.ToLower(text)
	for _, lang := range supportedLangs {
		score[lang] += langDiacriticBonus(lower, lang)
	}

	bestLang, bestScore, secondScore := "", 0, 0
	for _, lang := range supportedLangs {
		s := score[lang]
		if s > bestScore {
			secondScore = bestScore
			bestScore = s
			bestLang = lang
		} else if s > secondScore {
			secondScore = s
		}
	}
	if bestScore < 2 || bestScore == secondScore {
		return "" // insufficient signal or tie
	}
	return bestLang
}

var (
	answerLangMu    sync.Mutex
	answerLangState = make(map[string]string)
)

const maxAnswerLangSessions = 512

func stickySetLang(sessionID, lang string) {
	if sessionID == "" || lang == "" {
		return
	}
	answerLangMu.Lock()
	defer answerLangMu.Unlock()
	if _, exists := answerLangState[sessionID]; !exists && len(answerLangState) >= maxAnswerLangSessions {
		for k := range answerLangState {
			delete(answerLangState, k)
			break
		}
	}
	answerLangState[sessionID] = lang
}

func stickyGetLang(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	answerLangMu.Lock()
	defer answerLangMu.Unlock()
	return answerLangState[sessionID]
}

// langDetectV2 is VEDETTA_LANG_DETECT=2: the answer's language is decided by
// detectQuestionLanguage (Piano dopo audit 4.3 / D6). Set once at start.
var langDetectV2 atomic.Bool

// SetLangDetectV2 chooses how the answer's language is read from the question:
// today's count over the whole question, or detectQuestionLanguage.
func SetLangDetectV2(enabled bool) { langDetectV2.Store(enabled) }

// questionWordLists add to langWordLists, for detectQuestionLanguage only, what a
// question says its language with: common function words the lists miss, and the
// same words of asking in every language (explain, tell, summary, describe,
// compare, show, list, meaning, difference).
var questionWordLists = map[string][]string{
	"it": {"a", "da", "in", "su", "al", "allo", "alla", "ai", "agli", "alle", "dal", "dallo", "dalla", "dai", "dagli", "dalle", "mi", "ti", "si", "ci", "ne", "ma", "se", "o",
		"spiega", "spiegami", "spiegaci", "dimmi", "dicci", "riassumi", "riassumimi", "riassunto", "descrivi", "descrivimi", "confronta", "mostra", "mostrami", "elenca", "elencami", "significa", "significato", "differenza"},
	"en": {"a", "an", "in", "on", "at", "by", "it", "as", "or", "if", "so", "do", "does", "did", "me", "please",
		"explain", "tell", "summarize", "summarise", "summary", "describe", "compare", "show", "list", "mean", "means", "meaning", "difference"},
	"de": {"von", "vom", "zu", "zum", "zur", "im", "am", "an", "auf", "in", "es", "man", "sich", "mir", "mich", "bitte",
		"erkläre", "erklär", "erklären", "sag", "sage", "zusammenfassung", "fasse", "beschreibe", "vergleiche", "zeige", "liste", "bedeutet", "bedeutung", "unterschied"},
	"fr": {"à", "au", "en", "se", "ne", "y", "mais", "si", "me", "moi",
		"explique", "expliquez", "expliquer", "dis", "dites", "résume", "résumez", "résumé", "décris", "décrivez", "compare", "comparez", "montre", "liste", "signifie", "signification", "différence"},
	"es": {"a", "al", "lo", "le", "se", "su", "sus", "mi", "me", "te", "ni",
		"explica", "explícame", "explicame", "dime", "resume", "resumen", "describe", "compara", "muestra", "enumera", "significa", "significado", "diferencia"},
}

var questionLexicon = func() map[string]map[string]bool {
	m := make(map[string]map[string]bool, len(langWordLists))
	for lang, words := range langWordLists {
		m[lang] = make(map[string]bool, len(words))
		for _, w := range words {
			m[lang][w] = true
		}
		for _, w := range questionWordLists[lang] {
			m[lang][w] = true
		}
	}
	return m
}()

var quotedTextRe = regexp.MustCompile(`"[^"]*"|“[^”]*”|«[^»]*»`)

// detectQuestionLanguage is DetectTextLanguage for a question that may cite titles,
// names or phrases in another language ("spiegami the selfish gene di …"): the
// asking comes first, what is cited usually after. Words in quotes do not count; the
// first two words weigh 3, the rest of the first half 2, the second half 1; accents
// count only in the first half. Ties and weak signals give "", as today.
func detectQuestionLanguage(text string) string {
	tokens := tokenizeLang(quotedTextRe.ReplaceAllString(text, " "))
	if len(tokens) < 2 {
		return ""
	}
	score := make(map[string]int, len(supportedLangs))
	var firstHalf strings.Builder
	for i, t := range tokens {
		w := 1
		switch {
		case i < 2:
			w = 3
		case 2*i < len(tokens):
			w = 2
		}
		if 2*i < len(tokens) {
			firstHalf.WriteString(t + " ")
		}
		for _, lang := range supportedLangs {
			if questionLexicon[lang][t] {
				score[lang] += w
			}
		}
	}
	for _, lang := range supportedLangs {
		score[lang] += langDiacriticBonus(firstHalf.String(), lang)
	}
	bestLang, bestScore, secondScore := "", 0, 0
	for _, lang := range supportedLangs {
		switch s := score[lang]; {
		case s > bestScore:
			secondScore, bestScore, bestLang = bestScore, s, lang
		case s > secondScore:
			secondScore = s
		}
	}
	if bestScore < 2 || bestScore == secondScore {
		return ""
	}
	return bestLang
}

// ResolveAnswerLanguage determines the response language for a turn:
// explicit override > detected query language > sticky session language > UI locale.
// The UI locale is only a fallback; the answer follows the question.
func ResolveAnswerLanguage(sessionID, query, uiLang, override string) string {
	ui := NormalizeLanguage(uiLang)
	if override != "" {
		if code, ok := normalizeLangAlias(override); ok {
			stickySetLang(sessionID, code)
			return code
		}
	}
	detected := DetectTextLanguage(query)
	if langDetectV2.Load() {
		detected = detectQuestionLanguage(query)
	}
	if detected != "" {
		stickySetLang(sessionID, detected)
		return detected
	}
	if prev := stickyGetLang(sessionID); prev != "" {
		return prev
	}
	return ui
}

var stepMessages = map[string]map[string]string{
	"planning": {
		"it": "Analisi della richiesta e pianificazione sotto-ricerche",
		"en": "Analyzing query and planning sub-investigations",
		"de": "Analyse der Suchanfrage und Planung der Teiluntersuchungen",
		"fr": "Analyse de la requête et planification des sous-enquêtes",
		"es": "Análisis de la consulta y planificación de sub-investigaciones",
	},
	"planner_fallback": {
		"it": "Scomposizione della domanda non riuscita: applicata ricerca con keyword dirette",
		"en": "Question decomposition failed: applying direct keyword fallback",
		"de": "Zerlegung der Frage fehlgeschlagen: Direkte Keyword-Suche angewendet",
		"fr": "Décomposition de la question échouée: recherche par mots-clés appliquée",
		"es": "Descomposición de la pregunta fallida: aplicando búsqueda directa por palabras clave",
	},
	// V25-QA A5: the label Go puts where the writer declared its own reasoning.
	"deduction_label": {
		"it": "*Deduzione, non scritta nelle fonti:*",
		"en": "*Inference, not stated in the sources:*",
		"de": "*Schlussfolgerung, nicht in den Quellen:*",
		"fr": "*Déduction, absente des sources :*",
		"es": "*Deducción, no escrita en las fuentes:*",
	},
	"anchored_gap": {
		"it": "Il follow-up esce dall'evidenza raccolta: avvio una nuova ricerca",
		"en": "The follow-up is outside the collected evidence: starting a new search",
		"de": "Der Follow-up liegt außerhalb der gesammelten Belege: neue Suche wird gestartet",
		"fr": "La relance sort des preuves collectées : lancement d'une nouvelle recherche",
		"es": "El seguimiento queda fuera de la evidencia recogida: inicio una nueva búsqueda",
	},
	"error_no_answer": {
		"it": "Nessuna risposta generata: timeout o errore di sintesi.",
		"en": "No answer generated: timeout or synthesis error.",
		"de": "Keine Antwort erzeugt: Zeitüberschreitung oder Synthesefehler.",
		"fr": "Aucune réponse générée : délai dépassé ou erreur de synthèse.",
		"es": "No se generó respuesta: tiempo de espera agotado o error de síntesis.",
	},
	"deep_critic": {
		"it": "Analisi contraddizioni tra %d coppie (entità, attributo)",
		"en": "Analyzing contradictions across %d (entity, attribute) pairs",
		"de": "Widerspruchsanalyse über %d (Entität, Attribut)-Paare",
		"fr": "Analyse des contradictions sur %d paires (entité, attribut)",
		"es": "Análisis de contradicciones en %d pares (entidad, atributo)",
	},
	"sections": {
		"it": "Generazione report in %d sezioni tematiche",
		"en": "Generating report in %d thematic sections",
		"de": "Erstellung des Berichts in %d thematischen Abschnitten",
		"fr": "Génération du rapport en %d sections thématiques",
		"es": "Generación del informe en %d secciones temáticas",
	},
	"error_llm_unavailable": {
		"it": "Il modello linguistico non era disponibile o non ha risposto in tempo: la risposta non è stata scritta. Riprova più tardi.",
		"en": "The language model was unavailable or did not answer in time: the answer could not be written. Please try again later.",
		"de": "Das Sprachmodell war nicht verfügbar oder hat nicht rechtzeitig geantwortet: die Antwort konnte nicht geschrieben werden. Bitte später erneut versuchen.",
		"fr": "Le modèle de langage n'était pas disponible ou n'a pas répondu à temps : la réponse n'a pas pu être rédigée. Réessayez plus tard.",
		"es": "El modelo de lenguaje no estuvo disponible o no respondió a tiempo: no se pudo redactar la respuesta. Inténtalo de nuevo más tarde.",
	},
	// map R4: the research stops for several limits (time, calls, memory, repeats),
	// not only the time; said in plain words, as the interface says it on reopening
	"warning_partial": {
		"it": "Risposta parziale: la ricerca si è fermata prima della fine.",
		"en": "Partial answer: the research stopped before the end.",
		"de": "Teilantwort: die Recherche wurde vor dem Ende beendet.",
		"fr": "Réponse partielle : la recherche s'est arrêtée avant la fin.",
		"es": "Respuesta parcial: la búsqueda se detuvo antes del final.",
	},
	"warning_search_fallback": {
		"it": "Il servizio di ricerca scelto ha avuto problemi: alcune ricerche sono passate a quello di riserva.",
		"en": "The chosen search service had problems: some searches went to the backup one.",
		"de": "Der gewählte Suchdienst hatte Probleme: einige Suchen liefen über den Ersatzdienst.",
		"fr": "Le service de recherche choisi a eu des problèmes : certaines recherches sont passées par celui de secours.",
		"es": "El servicio de búsqueda elegido tuvo problemas: algunas búsquedas pasaron al de reserva.",
	},
	"link_unverified": {
		"it": "link non verificato",
		"en": "unverified link",
		"de": "nicht überprüfter Link",
		"fr": "lien non vérifié",
		"es": "enlace no verificado",
	},
	"loop_note_reader_fallback": {
		"it": "Alcune pagine sono state lette con %s perché %s non ci riusciva.",
		"en": "Some pages were read with %s because %s could not read them.",
		"de": "Einige Seiten hat %s gelesen, weil %s sie nicht lesen konnte.",
		"fr": "Certaines pages ont été lues avec %s parce que %s n'arrivait pas à les lire.",
		"es": "Algunas páginas se leyeron con %s porque %s no podía leerlas.",
	},
	"reader_name_http": {
		"it": "il lettore interno",
		"en": "the built-in reader",
		"de": "der eingebaute Leser",
		"fr": "le lecteur intégré",
		"es": "el lector integrado",
	},
	"loop_note_verified": {
		"it": "Link non presenti fra le pagine trovate, aperti da Vedetta per controllare che esistano: %d.",
		"en": "Links not among the pages found, opened by Vedetta to check that they exist: %d.",
		"de": "Links außerhalb der gefundenen Seiten, von Vedetta geöffnet, um zu prüfen, dass sie existieren: %d.",
		"fr": "Liens absents des pages trouvées, ouverts par Vedetta pour contrôler qu'ils existent : %d.",
		"es": "Enlaces fuera de las páginas encontradas, abiertos por Vedetta para comprobar que existen: %d.",
	},
	"loop_note_links": {
		"it": "Link nella risposta: %d. A pagine lette dal modello: %d. A pagine che non ha letto (ha visto al massimo un'anteprima o un brano): %d.",
		"en": "Links in the answer: %d. To pages the model read: %d. To pages it did not read (it saw a preview or a passage at most): %d.",
		"de": "Links in der Antwort: %d. Zu vom Modell gelesenen Seiten: %d. Zu Seiten, die es nicht gelesen hat (höchstens eine Vorschau oder ein Auszug): %d.",
		"fr": "Liens dans la réponse : %d. Vers des pages lues par le modèle : %d. Vers des pages qu'il n'a pas lues (au plus un aperçu ou un extrait) : %d.",
		"es": "Enlaces en la respuesta: %d. A páginas leídas por el modelo: %d. A páginas que no leyó (como mucho una vista previa o un fragmento): %d.",
	},
	"loop_link_check": {
		"it": "Vedetta apre le pagine citate che il modello non ha letto",
		"en": "Vedetta opens the cited pages the model did not read",
		"de": "Vedetta öffnet die zitierten Seiten, die das Modell nicht gelesen hat",
		"fr": "Vedetta ouvre les pages citées que le modèle n'a pas lues",
		"es": "Vedetta abre las páginas citadas que el modelo no leyó",
	},
	"loop_note_links_checked": {
		"it": "Link nella risposta: %d. A pagine lette dal modello: %d. A pagine che non ha letto, aperte e controllate da Vedetta: %d dicono la frase, %d la dicono in parte. Non controllate: %d. Indirizzi di progetti, che non citano nulla: %d.",
		"en": "Links in the answer: %d. To pages the model read: %d. To pages it did not read, opened and checked by Vedetta: %d say the sentence, %d say it in part. Not checked: %d. Project addresses, which cite nothing: %d.",
		"de": "Links in der Antwort: %d. Zu vom Modell gelesenen Seiten: %d. Zu nicht gelesenen Seiten, von Vedetta geöffnet und geprüft: %d enthalten den Satz, %d nur teilweise. Nicht geprüft: %d. Projektadressen, die nichts belegen: %d.",
		"fr": "Liens dans la réponse : %d. Vers des pages lues par le modèle : %d. Vers des pages qu'il n'a pas lues, ouvertes et contrôlées par Vedetta : %d disent la phrase, %d en partie. Non contrôlés : %d. Adresses de projets, qui ne citent rien : %d.",
		"es": "Enlaces en la respuesta: %d. A páginas leídas por el modelo: %d. A páginas que no leyó, abiertas y comprobadas por Vedetta: %d dicen la frase, %d en parte. Sin comprobar: %d. Direcciones de proyectos, que no citan nada: %d.",
	},
	"loop_note_links_removed": {
		"it": "Link tolti perché la pagina non dice la frase, o elencati come fonti senza essere stati letti: %d.",
		"en": "Links removed because the page does not say the sentence, or listed as sources without being read: %d.",
		"de": "Entfernte Links, weil die Seite den Satz nicht enthält oder ungelesen als Quelle aufgeführt war: %d.",
		"fr": "Liens retirés parce que la page ne dit pas la phrase, ou cités comme sources sans avoir été lus : %d.",
		"es": "Enlaces eliminados porque la página no dice la frase, o citados como fuentes sin haberlos leído: %d.",
	},
	"loop_note_check_off": {
		"it": "Le pagine citate non sono state controllate: il lettore scelto è a pagamento.",
		"en": "The cited pages were not checked: the chosen reader is paid.",
		"de": "Die zitierten Seiten wurden nicht geprüft: Der gewählte Reader ist kostenpflichtig.",
		"fr": "Les pages citées n'ont pas été contrôlées : le lecteur choisi est payant.",
		"es": "Las páginas citadas no se comprobaron: el lector elegido es de pago.",
	},
	"loop_link_review_note_one": {
		"it": "Vedetta ha aperto le pagine citate che il modello non aveva letto: 1 frase non corrispondeva ed è tornata al modello.",
		"en": "Vedetta opened the cited pages the model had not read: 1 sentence did not match and went back to the model.",
		"de": "Vedetta hat die zitierten, nicht gelesenen Seiten geöffnet: 1 Satz stimmte nicht überein und ging an das Modell zurück.",
		"fr": "Vedetta a ouvert les pages citées non lues par le modèle : 1 phrase ne correspondait pas et lui a été renvoyée.",
		"es": "Vedetta abrió las páginas citadas que el modelo no había leído: 1 frase no coincidía y volvió al modelo.",
	},
	"loop_link_review_note": {
		"it": "Vedetta ha aperto le pagine citate che il modello non aveva letto: %d frasi non corrispondevano e sono tornate al modello.",
		"en": "Vedetta opened the cited pages the model had not read: %d sentences did not match and went back to the model.",
		"de": "Vedetta hat die zitierten, nicht gelesenen Seiten geöffnet: %d Sätze stimmten nicht überein und gingen an das Modell zurück.",
		"fr": "Vedetta a ouvert les pages citées non lues par le modèle : %d phrases ne correspondaient pas et lui ont été renvoyées.",
		"es": "Vedetta abrió las páginas citadas que el modelo no había leído: %d frases no coincidían y volvieron al modelo.",
	},
	"loop_numbers_ok": {
		"it": "Numeri significativi della risposta (da 100 in su, decimali, migliaia): %d, tutti presenti nelle pagine lette o nelle anteprime.",
		"en": "Distinctive numbers in the answer (100 or more, decimals, thousands): %d, all present in the pages read or the previews.",
		"de": "Markante Zahlen der Antwort (ab 100, Dezimalzahlen, Tausender): %d, alle in den gelesenen Seiten oder Vorschauen vorhanden.",
		"fr": "Nombres significatifs de la réponse (100 ou plus, décimaux, milliers) : %d, tous présents dans les pages lues ou les aperçus.",
		"es": "Números significativos de la respuesta (100 o más, decimales, miles): %d, todos presentes en las páginas leídas o las vistas previas.",
	},
	"loop_numbers_missing": {
		"it": "Numeri che non compaiono nelle pagine lette né nelle anteprime, forse dalla memoria del modello o suoi calcoli: %d su %d.",
		"en": "Numbers not in the pages read nor in the previews, possibly from the model's memory or its own calculations: %d of %d.",
		"de": "Zahlen, die weder in den gelesenen Seiten noch in den Vorschauen stehen, vielleicht aus dem Modellwissen oder eigenen Berechnungen: %d von %d.",
		"fr": "Nombres absents des pages lues et des aperçus, peut-être issus de la mémoire du modèle ou de ses calculs : %d sur %d.",
		"es": "Números que no aparecen en las páginas leídas ni en las vistas previas, quizá de la memoria del modelo o de sus cálculos: %d de %d.",
	},
	"loop_numbers_more": {
		"it": "altre frasi: %d",
		"en": "more sentences: %d",
		"de": "weitere Sätze: %d",
		"fr": "autres phrases : %d",
		"es": "más frases: %d",
	},
	"loop_start": {
		"it": "Ricerca guidata dal modello: cerca e legge finché le pagine coprono la domanda",
		"en": "Model-driven research: searching and reading until the pages cover the question",
		"de": "Vom Modell geführte Recherche: Suchen und Lesen, bis die Seiten die Frage abdecken",
		"fr": "Recherche guidée par le modèle : recherche et lecture jusqu'à couvrir la question",
		"es": "Investigación guiada por el modelo: busca y lee hasta cubrir la pregunta",
	},
	"loop_search": {
		"it": "Cerco: %s",
		"en": "Searching: %s",
		"de": "Suche: %s",
		"fr": "Recherche : %s",
		"es": "Buscando: %s",
	},
	"loop_read": {
		"it": "Leggo: %s",
		"en": "Reading: %s",
		"de": "Lese: %s",
		"fr": "Lecture : %s",
		"es": "Leyendo: %s",
	},
	"loop_review": {
		"it": "Vedetta controlla la bozza: %d frasi con dettagli da verificare",
		"en": "Vedetta checks the draft: %d sentences with details to verify",
		"de": "Vedetta prüft den Entwurf: %d Sätze mit zu prüfenden Details",
		"fr": "Vedetta vérifie le brouillon : %d phrases avec des détails à vérifier",
		"es": "Vedetta revisa el borrador: %d frases con detalles por verificar",
	},
	"loop_quotes": {
		"it": "Vedetta chiede al modello da dove viene ogni frase",
		"en": "Vedetta asks the model where each sentence comes from",
		"de": "Vedetta fragt das Modell, woher jeder Satz stammt",
		"fr": "Vedetta demande au modèle d'où vient chaque phrase",
		"es": "Vedetta pregunta al modelo de dónde viene cada frase",
	},
	"loop_quotes_found": {
		"it": "Frasi con una citazione ritrovata nelle pagine lette: %d di %d.",
		"en": "Sentences with a quote found in the pages read: %d of %d.",
		"de": "Sätze mit einem in den gelesenen Seiten wiedergefundenen Zitat: %d von %d.",
		"fr": "Phrases avec une citation retrouvée dans les pages lues : %d sur %d.",
		"es": "Frases con una cita encontrada en las páginas leídas: %d de %d.",
	},
	"loop_quotes_notfound": {
		"it": "Citazioni indicate dal modello e non ritrovate: %d.",
		"en": "Quotes given by the model and not found: %d.",
		"de": "Vom Modell angegebene und nicht wiedergefundene Zitate: %d.",
		"fr": "Citations indiquées par le modèle et non retrouvées : %d.",
		"es": "Citas indicadas por el modelo y no encontradas: %d.",
	},
	"loop_quotes_short": {
		"it": "Frasi con una citazione di meno di %d parole, troppo corta per fare da prova e non cercata: %d.",
		"en": "Sentences with a quote under %d words, too short to prove anything and not looked for: %d.",
		"de": "Sätze mit einem Zitat unter %d Wörtern, zu kurz als Beleg und nicht gesucht: %d.",
		"fr": "Phrases avec une citation de moins de %d mots, trop courte pour servir de preuve et non cherchée : %d.",
		"es": "Frases con una cita de menos de %d palabras, demasiado corta para servir de prueba y no buscada: %d.",
	},
	"deep_thought_step": {
		"it": "Pensiero Profondo ci sta pensando. Ci vorranno sette secondi e mezzo",
		"en": "Deep Thought is thinking about it. It will take seven and a half seconds",
		"de": "Deep Thought denkt darüber nach. Es wird siebeneinhalb Sekunden dauern",
		"fr": "Deep Thought y réfléchit. Il lui faudra sept secondes et demie",
		"es": "Deep Thought lo está pensando. Tardará siete segundos y medio",
	},
	"deep_thought_note": {
		"it": "Ha risposto Pensiero Profondo, in sette secondi e mezzo: la prima volta gli erano serviti sette milioni e mezzo di anni. Nessuna ricerca, nessuna pagina letta, nessun modello. Si spegne in Impostazioni → Sperimentale.",
		"en": "Deep Thought answered, in seven and a half seconds: the first time it needed seven and a half million years. No search, no page read, no model. To turn it off: Settings → Experimental.",
		"de": "Deep Thought hat geantwortet, in siebeneinhalb Sekunden: Beim ersten Mal brauchte er siebeneinhalb Millionen Jahre. Keine Suche, keine gelesene Seite, kein Modell. Ausschalten: Settings → Experimental.",
		"fr": "Deep Thought a répondu, en sept secondes et demie : la première fois, il lui avait fallu sept millions et demi d'années. Aucune recherche, aucune page lue, aucun modèle. Pour l'éteindre : Settings → Experimental.",
		"es": "Respondió Deep Thought, en siete segundos y medio: la primera vez necesitó siete millones y medio de años. Ninguna búsqueda, ninguna página leída, ningún modelo. Se apaga en Settings → Experimental.",
	},
	"loop_basis_quotes": {
		"it": "La risposta non ha link alle pagine lette: le frasi ritrovate lì sono elencate in «%s».",
		"en": "The answer has no links to the pages read: the sentences found there are listed under “%s”.",
		"de": "Die Antwort enthält keine Links zu den gelesenen Seiten: Die dort wiedergefundenen Sätze stehen unter „%s“.",
		"fr": "La réponse n'a pas de liens vers les pages lues : les phrases qui y ont été retrouvées sont listées dans « %s ».",
		"es": "La respuesta no tiene enlaces a las páginas leídas: las frases encontradas allí están en «%s».",
	},
	"loop_quotes_unusable": {
		"it": "Vedetta ha chiesto al modello da dove viene ogni frase, ma non ha avuto una risposta utilizzabile.",
		"en": "Vedetta asked the model where each sentence comes from, but did not get a usable reply.",
		"de": "Vedetta hat das Modell gefragt, woher jeder Satz stammt, aber keine brauchbare Antwort erhalten.",
		"fr": "Vedetta a demandé au modèle d'où vient chaque phrase, mais n'a pas reçu de réponse exploitable.",
		"es": "Vedetta preguntó al modelo de dónde viene cada frase, pero no obtuvo una respuesta utilizable.",
	},
	// the list of the quotes at the end of the notes (Piano fase 6 chiusura, B2)
	"loop_quotes_list_title": {
		"it": "Citazioni",
		"en": "Quotes",
		"de": "Zitate",
		"fr": "Citations",
		"es": "Citas",
	},
	"loop_quotes_list_intro": {
		"it": "Per ogni frase con una citazione ritrovata: l'inizio della frase, le parole ritrovate sulla pagina e il numero della pagina in «%s».",
		"en": "For each sentence with a quote found: the start of the sentence, the words found on the page and the page's number in “%s”.",
		"de": "Für jeden Satz mit einem wiedergefundenen Zitat: der Anfang des Satzes, die auf der Seite wiedergefundenen Worte und die Nummer der Seite in „%s“.",
		"fr": "Pour chaque phrase avec une citation retrouvée : le début de la phrase, les mots retrouvés sur la page et le numéro de la page dans « %s ».",
		"es": "Para cada frase con una cita encontrada: el comienzo de la frase, las palabras encontradas en la página y el número de la página en «%s».",
	},
	"loop_quotes_list_none_one": {
		"it": "Per un'altra frase il modello non ha indicato una citazione.",
		"en": "For one more sentence the model gave no quote.",
		"de": "Für einen weiteren Satz hat das Modell kein Zitat angegeben.",
		"fr": "Pour une autre phrase, le modèle n'a indiqué aucune citation.",
		"es": "Para otra frase el modelo no indicó ninguna cita.",
	},
	"loop_quotes_list_none": {
		"it": "Per altre %d frasi il modello non ha indicato una citazione.",
		"en": "For %d more sentences the model gave no quote.",
		"de": "Für %d weitere Sätze hat das Modell kein Zitat angegeben.",
		"fr": "Pour %d autres phrases, le modèle n'a indiqué aucune citation.",
		"es": "Para otras %d frases el modelo no indicó ninguna cita.",
	},
	"loop_quotes_list_page": {
		"it": "pagina letta %d",
		"en": "page read %d",
		"de": "gelesene Seite %d",
		"fr": "page lue %d",
		"es": "página leída %d",
	},
	// the numbers are looked for in the paragraph around the quote (passageText), not in the quoted words alone
	"loop_quotes_list_out": {
		"it": "numeri della frase che la pagina non ha, nel paragrafo della citazione: %s",
		"en": "numbers of the sentence the page does not have, in the quote's paragraph: %s",
		"de": "Zahlen des Satzes, die die Seite im Absatz des Zitats nicht enthält: %s",
		"fr": "nombres de la phrase absents de la page, dans le paragraphe de la citation : %s",
		"es": "números de la frase que la página no tiene, en el párrafo de la cita: %s",
	},
	"loop_quotes_list_out_many": {
		"it": "numeri della frase che le pagine non hanno, nei paragrafi delle citazioni: %s",
		"en": "numbers of the sentence the pages do not have, in the quotes' paragraphs: %s",
		"de": "Zahlen des Satzes, die die Seiten in den Absätzen der Zitate nicht enthalten: %s",
		"fr": "nombres de la phrase absents des pages, dans les paragraphes des citations : %s",
		"es": "números de la frase que las páginas no tienen, en los párrafos de las citas: %s",
	},
	"loop_grant": {
		"it": "La ricerca trova ancora pagine nuove: Vedetta concede altre %d ricerche e letture",
		"en": "The research is still finding new pages: Vedetta allows %d more searches and readings",
		"de": "Die Recherche findet noch neue Seiten: Vedetta erlaubt %d weitere Suchen und Lektüren",
		"fr": "La recherche trouve encore de nouvelles pages : Vedetta accorde %d recherches et lectures de plus",
		"es": "La investigación sigue encontrando páginas nuevas: Vedetta concede %d búsquedas y lecturas más",
	},
	"loop_note_grant": {
		"it": "Arrivata al limite, la ricerca trovava ancora pagine nuove: Vedetta ha concesso altre %d ricerche e letture.",
		"en": "At its limit the research was still finding new pages: Vedetta allowed %d more searches and readings.",
		"de": "Am Limit fand die Recherche noch neue Seiten: Vedetta hat %d weitere Suchen und Lektüren erlaubt.",
		"fr": "Arrivée à sa limite, la recherche trouvait encore de nouvelles pages : Vedetta a accordé %d recherches et lectures de plus.",
		"es": "Al llegar al límite, la investigación seguía encontrando páginas nuevas: Vedetta concedió %d búsquedas y lecturas más.",
	},
	"loop_push": {
		"it": "Vedetta chiede al modello se c'è altro da cercare",
		"en": "Vedetta asks the model whether there is more to look for",
		"de": "Vedetta fragt das Modell, ob es noch etwas zu suchen gibt",
		"fr": "Vedetta demande au modèle s'il reste quelque chose à chercher",
		"es": "Vedetta pregunta al modelo si queda algo por buscar",
	},
	"loop_language": {
		"it": "Vedetta controlla la lingua: %d righe con parole da riscrivere",
		"en": "Vedetta checks the language: %d lines with words to rewrite",
		"de": "Vedetta prüft die Sprache: %d Zeilen mit umzuschreibenden Wörtern",
		"fr": "Vedetta vérifie la langue : %d lignes avec des mots à réécrire",
		"es": "Vedetta revisa el idioma: %d líneas con palabras por reescribir",
	},
	"loop_claims": {
		"it": "Vedetta controlla ciò che la risposta dice di aver fatto: %d righe",
		"en": "Vedetta checks what the answer says it did: %d lines",
		"de": "Vedetta prüft, was die Antwort über ihre Arbeit sagt: %d Zeilen",
		"fr": "Vedetta vérifie ce que la réponse dit avoir fait : %d lignes",
		"es": "Vedetta revisa lo que la respuesta dice haber hecho: %d líneas",
	},
	"loop_claims_note_one": {
		"it": "Vedetta ha fatto correggere 1 frase che descriveva più controlli o letture di quelli fatti davvero.",
		"en": "Vedetta had 1 sentence corrected that described more checks or reading than were actually done.",
		"de": "Vedetta ließ 1 Satz korrigieren, der mehr Prüfungen oder Lektüre beschrieb als tatsächlich erfolgt.",
		"fr": "Vedetta a fait corriger 1 phrase qui décrivait plus de vérifications ou de lectures que celles réellement faites.",
		"es": "Vedetta hizo corregir 1 frase que describía más comprobaciones o lecturas de las realmente hechas.",
	},
	"loop_claims_note": {
		"it": "Vedetta ha fatto correggere %d frasi che descrivevano più controlli o letture di quelli fatti davvero.",
		"en": "Vedetta had %d sentences corrected that described more checks or reading than were actually done.",
		"de": "Vedetta ließ %d Sätze korrigieren, die mehr Prüfungen oder Lektüre beschrieben als tatsächlich erfolgt.",
		"fr": "Vedetta a fait corriger %d phrases qui décrivaient plus de vérifications ou de lectures que celles réellement faites.",
		"es": "Vedetta hizo corregir %d frases que describían más comprobaciones o lecturas de las realmente hechas.",
	},
	"loop_review_note_one": {
		"it": "Prima di consegnarla, Vedetta ha rimandato al modello la bozza: 1 frase aveva numeri o nomi che non stavano in nessuna pagina letta, da verificare o togliere.",
		"en": "Before delivering it, Vedetta sent the draft back to the model: 1 sentence had numbers or names found in no page read, to verify or remove.",
		"de": "Vor der Auslieferung hat Vedetta den Entwurf an das Modell zurückgeschickt: 1 Satz enthielt Zahlen oder Namen, die auf keiner gelesenen Seite standen, zu prüfen oder zu entfernen.",
		"fr": "Avant de la livrer, Vedetta a renvoyé le brouillon au modèle : 1 phrase contenait des nombres ou des noms absents des pages lues, à vérifier ou supprimer.",
		"es": "Antes de entregarla, Vedetta devolvió el borrador al modelo: 1 frase tenía números o nombres que no estaban en ninguna página leída, para verificar o eliminar.",
	},
	"loop_review_note": {
		"it": "Prima di consegnarla, Vedetta ha rimandato al modello la bozza: %d frasi avevano numeri o nomi che non stavano in nessuna pagina letta, da verificare o togliere.",
		"en": "Before delivering it, Vedetta sent the draft back to the model: %d sentences had numbers or names found in no page read, to verify or remove.",
		"de": "Vor der Auslieferung hat Vedetta den Entwurf an das Modell zurückgeschickt: %d Sätze enthielten Zahlen oder Namen, die auf keiner gelesenen Seite standen, zum Prüfen oder Entfernen.",
		"fr": "Avant de la livrer, Vedetta a renvoyé le brouillon au modèle : %d phrases contenaient des nombres ou des noms absents des pages lues, à vérifier ou supprimer.",
		"es": "Antes de entregarla, Vedetta devolvió el borrador al modelo: %d frases tenían números o nombres que no estaban en ninguna página leída, para verificar o eliminar.",
	},
	"loop_write": {
		"it": "Scrittura della risposta da %d pagine lette",
		"en": "Writing the answer from %d pages read",
		"de": "Antwort aus %d gelesenen Seiten wird geschrieben",
		"fr": "Rédaction de la réponse à partir de %d pages lues",
		"es": "Redactando la respuesta a partir de %d páginas leídas",
	},
	"loop_pages_read": {
		"it": "Pagine lette",
		"en": "Pages read",
		"de": "Gelesene Seiten",
		"fr": "Pages lues",
		"es": "Páginas leídas",
	},
	"loop_note": {
		"it": "Ricerca guidata dal modello: %s, %s, %s.",
		"en": "Model-driven research: %s, %s, %s.",
		"de": "Vom Modell geführte Recherche: %s, %s, %s.",
		"fr": "Recherche guidée par le modèle : %s, %s, %s.",
		"es": "Investigación guiada por el modelo: %s, %s, %s.",
	},
	"loop_page_partial": {
		"it": "letta solo in parte",
		"en": "read only in part",
		"de": "nur teilweise gelesen",
		"fr": "lue en partie seulement",
		"es": "leída solo en parte",
	},
	"loop_page_removed": {
		"it": "tolta: non entrava nella memoria del modello",
		"en": "removed: it did not fit in the model's memory",
		"de": "entfernt: passte nicht in den Speicher des Modells",
		"fr": "retirée : elle ne tenait pas dans la mémoire du modèle",
		"es": "quitada: no cabía en la memoria del modelo",
	},
	"loop_note_context": {
		"it": "La conversazione ha riempito la memoria del modello (%s token): la ricerca si è chiusa lì.",
		"en": "The conversation filled the model's memory (%s tokens): the research stopped there.",
		"de": "Das Gespräch hat den Speicher des Modells gefüllt (%s Tokens): die Recherche endete dort.",
		"fr": "La conversation a rempli la mémoire du modèle (%s jetons) : la recherche s'est arrêtée là.",
		"es": "La conversación llenó la memoria del modelo (%s tokens): la investigación se detuvo ahí.",
	},
	"loop_note_context_default": {
		"it": "(memoria non dichiarata dal server: Vedetta ha usato un valore prudente)",
		"en": "(memory not stated by the server: Vedetta used a prudent value)",
		"de": "(Speicher vom Server nicht angegeben: Vedetta hat einen vorsichtigen Wert verwendet)",
		"fr": "(mémoire non indiquée par le serveur : Vedetta a utilisé une valeur prudente)",
		"es": "(memoria no indicada por el servidor: Vedetta usó un valor prudente)",
	},
	"loop_note_refused": {
		"it": "Il server ha rifiutato la conversazione perché troppo lunga: Vedetta ha tolto %s e ha chiesto la risposta con il resto.",
		"en": "The server refused the conversation as too long: Vedetta took out %s and asked for the answer with the rest.",
		"de": "Der Server hat das Gespräch als zu lang abgelehnt: Vedetta hat %s entfernt und die Antwort mit dem Rest angefordert.",
		"fr": "Le serveur a refusé la conversation, trop longue : Vedetta a retiré %s et a demandé la réponse avec le reste.",
		"es": "El servidor rechazó la conversación por demasiado larga: Vedetta quitó %s y pidió la respuesta con el resto.",
	},
	"loop_note_partial": {
		"it": "Pagine troppo lunghe per questa conversazione, lette solo in parte: %d.",
		"en": "Pages too long for this conversation, read only in part: %d.",
		"de": "Für dieses Gespräch zu lange Seiten, nur teilweise gelesen: %d.",
		"fr": "Pages trop longues pour cette conversation, lues en partie seulement : %d.",
		"es": "Páginas demasiado largas para esta conversación, leídas solo en parte: %d.",
	},
	"error_tools_required": {
		"it": "Il servizio del modello non accetta gli strumenti: Vedetta ha bisogno di un modello che sappia usarli per cercare e leggere. Scegline un altro nelle impostazioni.",
		"en": "The model service does not accept tools: Vedetta needs a model that can use them to search and read. Choose another one in the settings.",
		"de": "Der Modelldienst akzeptiert keine Werkzeuge: Vedetta braucht ein Modell, das sie zum Suchen und Lesen benutzen kann. Wähle in den Einstellungen ein anderes.",
		"fr": "Le service du modèle n'accepte pas les outils : Vedetta a besoin d'un modèle capable de les utiliser pour chercher et lire. Choisis-en un autre dans les réglages.",
		"es": "El servicio del modelo no acepta herramientas: Vedetta necesita un modelo que sepa usarlas para buscar y leer. Elige otro en los ajustes.",
	},
	"error_resume_unavailable": {
		"it": "Riprendere una ricerca interrotta non è più possibile per ora: fai di nuovo la domanda.",
		"en": "Resuming an interrupted search is not available for now: ask the question again.",
		"de": "Eine unterbrochene Suche fortzusetzen ist vorerst nicht möglich: Stelle die Frage erneut.",
		"fr": "Reprendre une recherche interrompue n'est pas possible pour l'instant : pose de nouveau la question.",
		"es": "Reanudar una búsqueda interrumpida no es posible por ahora: vuelve a hacer la pregunta.",
	},
	"loop_page_read_today": {
		"it": "letta oggi alle %s",
		"en": "read today at %s",
		"de": "heute um %s gelesen",
		"fr": "lue aujourd'hui à %s",
		"es": "leída hoy a las %s",
	},
	"loop_page_read_on_at": {
		"it": "letta il %s alle %s",
		"en": "read on %s at %s",
		"de": "gelesen am %s um %s",
		"fr": "lue le %s à %s",
		"es": "leída el %s a las %s",
	},
	"loop_page_read_on": {
		"it": "letta il %s",
		"en": "read on %s",
		"de": "gelesen am %s",
		"fr": "lue le %s",
		"es": "leída el %s",
	},
	"loop_note_search_fallback": {
		"it": "Il servizio di ricerca principale non ha sempre risposto: alcune ricerche sono state fatte con il servizio di riserva.",
		"en": "The main search service did not always answer: some searches were made with the backup service.",
		"de": "Der Hauptsuchdienst hat nicht immer geantwortet: Einige Suchen liefen über den Ersatzdienst.",
		"fr": "Le service de recherche principal n'a pas toujours répondu : certaines recherches ont été faites avec le service de secours.",
		"es": "El servicio de búsqueda principal no siempre respondió: algunas búsquedas se hicieron con el servicio de reserva.",
	},
	"loop_note_engines_silent": {
		"it": "Motori di ricerca che non hanno risposto ad alcune ricerche: %s.",
		"en": "Search engines that did not answer some searches: %s.",
		"de": "Suchmaschinen, die auf einige Suchen nicht geantwortet haben: %s.",
		"fr": "Moteurs de recherche qui n'ont pas répondu à certaines recherches : %s.",
		"es": "Motores de búsqueda que no respondieron a algunas búsquedas: %s.",
	},
	"loop_review_skipped": {
		"it": "La scrittura si è interrotta: Vedetta non ha potuto rimandare la bozza al modello per i numeri e i nomi non trovati nelle pagine.",
		"en": "The writing was cut: Vedetta could not send the draft back to the model for the numbers and names not found in the pages.",
		"de": "Das Schreiben wurde unterbrochen: Vedetta konnte den Entwurf nicht wegen der in den Seiten nicht gefundenen Zahlen und Namen an das Modell zurückschicken.",
		"fr": "La rédaction s'est interrompue : Vedetta n'a pas pu renvoyer le brouillon au modèle pour les nombres et les noms introuvables dans les pages.",
		"es": "La escritura se interrumpió: Vedetta no pudo devolver el borrador al modelo por los números y nombres no encontrados en las páginas.",
	},
	"loop_review_unfinished": {
		"it": "Ma la riscrittura chiesta non si è conclusa: questa è la prima bozza, e le frasi rimandate sono rimaste come erano.",
		"en": "But the rewrite asked for did not finish: this is the first draft, and the sentences sent back are as they were.",
		"de": "Aber die verlangte Überarbeitung wurde nicht abgeschlossen: Dies ist der erste Entwurf, und die zurückgeschickten Sätze sind unverändert.",
		"fr": "Mais la réécriture demandée n'a pas abouti : ceci est le premier brouillon, et les phrases renvoyées sont restées telles quelles.",
		"es": "Pero la reescritura pedida no se completó: este es el primer borrador, y las frases devueltas quedaron como estaban.",
	},
	"loop_note_pdf_partial": {
		"it": "PDF letti solo in parte: %d.",
		"en": "PDFs read only in part: %d.",
		"de": "Nur teilweise gelesene PDFs: %d.",
		"fr": "PDF lus en partie seulement : %d.",
		"es": "PDF leídos solo en parte: %d.",
	},
	"loop_note_dropped": {
		"it": "Link tolti perché non compaiono fra le pagine trovate e non si sono potuti verificare: %d.",
		"en": "Links removed because they are not among the pages found and could not be verified: %d.",
		"de": "Entfernte Links, die nicht unter den gefundenen Seiten waren und nicht überprüft werden konnten: %d.",
		"fr": "Liens retirés, absents des pages trouvées et impossibles à vérifier : %d.",
		"es": "Enlaces eliminados, fuera de las páginas encontradas y sin poder verificarse: %d.",
	},
	"wrap_up": {
		"it": "Completamento incrementale delle sezioni finali...",
		"en": "Completing missing sections and checklist...",
		"de": "Vervollständigung der fehlenden Abschnitte...",
		"fr": "Finalisation des sections manquantes...",
		"es": "Completando las secciones restantes...",
	},
}

// authoritativeSuffix holds, per language, the words appended to a normative gap
// re-query to steer it toward official sources. Data, not code: adding a language is one
// line. Languages without an entry use English.
var authoritativeSuffix = map[string]string{
	"it": " guida normativa ufficiale",
	"de": " offizieller leitfaden richtlinie",
	"fr": " guide officiel reglementation",
	"es": " guia oficial normativa",
	"en": " official guide documentation",
}

// StepMessage retrieves the localized step message formatted with optional arguments.
func StepMessage(lang string, key string, args ...interface{}) string {
	l := NormalizeLanguage(lang)
	templates, ok := stepMessages[key]
	if !ok {
		return key
	}
	tpl, has := templates[l]
	if !has {
		tpl = templates["en"]
		if tpl == "" {
			tpl = templates["it"]
		}
	}
	if len(args) > 0 {
		return fmt.Sprintf(tpl, args...)
	}
	return tpl
}
