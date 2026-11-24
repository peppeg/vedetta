package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"

	"vedetta/internal/llm"
)

// V25-T7.1b (docs/SPEC_T71B_AVVISO_LLM.md): when the language model does not
// answer, every stage takes its fallback. The session records which stages
// ran without the model, so the answer can say it (qualitatively, never how
// much was lost) and its provenance becomes "degraded".

type llmStage string

const (
	stagePlanner    llmStage = "planner"
	stageExtraction llmStage = "extraction"
	stageCritic     llmStage = "critic"
	stageSections   llmStage = "sections"
	stageSynthesis  llmStage = "synthesis"
)

// stageOrder is the order stages are listed in, the order they run in.
var stageOrder = []llmStage{stagePlanner, stageExtraction, stageCritic, stageSections, stageSynthesis}

type llmCause string

const (
	causeUnavailable llmCause = "unavailable" // the server answered 429/5xx or dropped the connection
	causeTimeout     llmCause = "timeout"     // the stage's time limit passed
	causeMixed       llmCause = "mixed"       // both, for the same stage or across stages
)

// LLMTroubleCount is how often a stage ran without the model, and why
// (AnswerMeta v5, for measurements only: never shown to the user).
type LLMTroubleCount struct {
	Count int      `json:"count"`
	Cause llmCause `json:"cause"`
}

// llmTrouble is the per-session record. Safe for concurrent use.
type llmTrouble struct {
	mu     sync.Mutex
	stages map[llmStage]*LLMTroubleCount
}

func newLLMTrouble() *llmTrouble {
	return &llmTrouble{stages: make(map[llmStage]*LLMTroubleCount)}
}

func mergeCause(a, b llmCause) llmCause {
	if a == "" || a == b {
		return b
	}
	return causeMixed
}

// note records that stage ran without the model because of err. Errors that
// are not the model failing to answer (unreadable JSON, a caller that went
// away) are not recorded: the stages already handle them as before.
func (t *llmTrouble) note(stage llmStage, err error) {
	cause, ok := classifyLLMErr(err)
	if t == nil || !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.stages[stage]
	if c == nil {
		c = &LLMTroubleCount{}
		t.stages[stage] = c
	}
	c.Count++
	c.Cause = mergeCause(c.Cause, cause)
}

// snapshot returns a copy of the record, nil when nothing went wrong.
func (t *llmTrouble) snapshot() map[string]LLMTroubleCount {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.stages) == 0 {
		return nil
	}
	out := make(map[string]LLMTroubleCount, len(t.stages))
	for s, c := range t.stages {
		out[string(s)] = *c
	}
	return out
}

func (t *llmTrouble) empty() bool {
	return t.snapshot() == nil
}

// classifyLLMErr tells whether err means the model did not answer, and why.
func classifyLLMErr(err error) (llmCause, bool) {
	if err == nil || errors.Is(err, context.Canceled) {
		return "", false
	}
	var se *llm.StatusError
	if errors.As(err, &se) || errors.Is(err, llm.ErrContextExceeded) {
		return causeUnavailable, true // Kilo's silent refusal carries no status
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return causeTimeout, true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return causeTimeout, true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) || errors.Is(err, io.ErrUnexpectedEOF) {
		return causeUnavailable, true
	}
	return "", false
}

type llmTroubleKey struct{}

func withLLMTrouble(ctx context.Context, t *llmTrouble) context.Context {
	return context.WithValue(ctx, llmTroubleKey{}, t)
}

// noteLLMTrouble records on the session carried by ctx; a no-op without one.
func noteLLMTrouble(ctx context.Context, stage llmStage, err error) {
	if t, ok := ctx.Value(llmTroubleKey{}).(*llmTrouble); ok {
		t.note(stage, err)
	}
}

// overallCause is the cause across all stages.
func overallCause(rec map[string]LLMTroubleCount) llmCause {
	var c llmCause
	for _, v := range rec {
		c = mergeCause(c, v.Cause)
	}
	return c
}

// troubleStages lists the recorded stages in pipeline order.
func troubleStages(rec map[string]LLMTroubleCount) []llmStage {
	var out []llmStage
	for _, s := range stageOrder {
		if _, ok := rec[string(s)]; ok {
			out = append(out, s)
		}
	}
	return out
}

var llmTroubleSubject = map[string]map[llmCause]string{
	"it": {
		causeUnavailable: "Il modello linguistico non era disponibile durante alcune fasi della ricerca",
		causeTimeout:     "Il modello linguistico non ha risposto in tempo durante alcune fasi della ricerca",
		causeMixed:       "Il modello linguistico non era disponibile o non ha risposto in tempo durante alcune fasi della ricerca",
	},
	"en": {
		causeUnavailable: "The language model was unavailable during some steps of the research",
		causeTimeout:     "The language model did not answer in time during some steps of the research",
		causeMixed:       "The language model was unavailable or did not answer in time during some steps of the research",
	},
	"de": {
		causeUnavailable: "Das Sprachmodell war während einiger Rechercheschritte nicht verfügbar",
		causeTimeout:     "Das Sprachmodell hat während einiger Rechercheschritte nicht rechtzeitig geantwortet",
		causeMixed:       "Das Sprachmodell war während einiger Rechercheschritte nicht verfügbar oder hat nicht rechtzeitig geantwortet",
	},
	"fr": {
		causeUnavailable: "Le modèle de langage n'était pas disponible pendant certaines étapes de la recherche",
		causeTimeout:     "Le modèle de langage n'a pas répondu à temps pendant certaines étapes de la recherche",
		causeMixed:       "Le modèle de langage n'était pas disponible ou n'a pas répondu à temps pendant certaines étapes de la recherche",
	},
	"es": {
		causeUnavailable: "El modelo de lenguaje no estuvo disponible durante algunas fases de la investigación",
		causeTimeout:     "El modelo de lenguaje no respondió a tiempo durante algunas fases de la investigación",
		causeMixed:       "El modelo de lenguaje no estuvo disponible o no respondió a tiempo durante algunas fases de la investigación",
	},
}

var llmTroubleStageText = map[string]map[llmStage]string{
	"it": {
		stagePlanner:    "le ricerche sono state pianificate senza il modello, con parole chiave dirette",
		stageExtraction: "alcune fonti lette non sono state analizzate",
		stageCritic:     "le divergenze tra fonti non sono state verificate",
		stageSections:   "le sezioni sono state organizzate senza il modello",
		stageSynthesis:  "la scrittura della risposta è stata interrotta",
	},
	"en": {
		stagePlanner:    "the searches were planned without the model, with direct keywords",
		stageExtraction: "some sources that were read were not analysed",
		stageCritic:     "disagreements between sources were not checked",
		stageSections:   "the sections were organised without the model",
		stageSynthesis:  "writing the answer was interrupted",
	},
	"de": {
		stagePlanner:    "die Suchen wurden ohne das Modell geplant, mit direkten Stichwörtern",
		stageExtraction: "einige gelesene Quellen wurden nicht ausgewertet",
		stageCritic:     "Widersprüche zwischen Quellen wurden nicht geprüft",
		stageSections:   "die Abschnitte wurden ohne das Modell gegliedert",
		stageSynthesis:  "das Schreiben der Antwort wurde unterbrochen",
	},
	"fr": {
		stagePlanner:    "les recherches ont été planifiées sans le modèle, avec des mots-clés directs",
		stageExtraction: "certaines sources lues n'ont pas été analysées",
		stageCritic:     "les divergences entre sources n'ont pas été vérifiées",
		stageSections:   "les sections ont été organisées sans le modèle",
		stageSynthesis:  "la rédaction de la réponse a été interrompue",
	},
	"es": {
		stagePlanner:    "las búsquedas se planificaron sin el modelo, con palabras clave directas",
		stageExtraction: "algunas fuentes leídas no se analizaron",
		stageCritic:     "las divergencias entre fuentes no se verificaron",
		stageSections:   "las secciones se organizaron sin el modelo",
		stageSynthesis:  "la redacción de la respuesta se interrumpió",
	},
}

var llmTroubleTitle = map[string]string{
	"it": "Limiti di questa risposta.",
	"en": "Limits of this answer.",
	"de": "Grenzen dieser Antwort.",
	"fr": "Limites de cette réponse.",
	"es": "Límites de esta respuesta.",
}

var llmTroubleClosing = map[string]string{
	"it": "Le parti interessate si basano su meno elementi del solito. Riprovare più tardi può dare una risposta più completa.",
	"en": "The affected parts rest on less evidence than usual. Trying again later may give a more complete answer.",
	"de": "Die betroffenen Teile stützen sich auf weniger Belege als üblich. Ein späterer neuer Versuch kann eine vollständigere Antwort liefern.",
	"fr": "Les parties concernées reposent sur moins d'éléments que d'habitude. Réessayer plus tard peut donner une réponse plus complète.",
	"es": "Las partes afectadas se basan en menos elementos de lo habitual. Volver a intentarlo más tarde puede dar una respuesta más completa.",
}

// llmTroubleNote renders the deterministic "Limits of this answer" paragraph,
// "" when nothing went wrong. It names the stages in words and never counts.
func llmTroubleNote(rec map[string]LLMTroubleCount, lang string) string {
	stages := troubleStages(rec)
	if len(stages) == 0 {
		return ""
	}
	l := NormalizeLanguage(lang)
	if _, ok := llmTroubleTitle[l]; !ok {
		l = "en"
	}
	parts := make([]string, 0, len(stages))
	for _, s := range stages {
		parts = append(parts, "*"+llmTroubleStageText[l][s]+"*")
	}
	sep := ": "
	if l == "fr" {
		sep = " : "
	}
	return fmt.Sprintf("\n\n> **%s** %s%s%s. %s", llmTroubleTitle[l], llmTroubleSubject[l][overallCause(rec)], sep,
		strings.Join(parts, "; "), llmTroubleClosing[l])
}

// formatLLMTrouble is the compact form for logs: "extraction:3/unavailable,critic:1/timeout".
func formatLLMTrouble(rec map[string]LLMTroubleCount) string {
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d/%s", k, rec[k].Count, rec[k].Cause))
	}
	return strings.Join(parts, ",")
}
