package agent

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"vedetta/internal/search"
)

// Reconciler is an instance-scoped streaming parser that converts [[fact:ID]] tags
// in flight into sequential [1], [2] citations and builds the final verified bibliography.
// It tracks dropped phantom tags locally without race conditions.
//
// With WithSnippets it also turns [[snip:Sn]] tags into [S1], [S2] citations to the
// search previews the writer was shown, and with WithSourceURLs it checks every link
// the writer prints against the session sources (V25-T3.5).
//
// The writer's [[deduction]] tag becomes a fixed label in the answer language,
// written by Go and counted (V25-QA A5).
type Reconciler struct {
	store           *FactStore
	citationMap     map[string]int // "F1" -> 1, "F3" -> 2 (order of appearance)
	citations       []FactCard     // Ordered list of unique cited facts
	droppedPhantoms int            // Request-local counter
	buffer          string         // Small lookahead buffer for chunk boundaries
	lastOut         byte           // last byte emitted (0 before any output), for word-start checks

	snippets               map[string]search.Snippet // "S4" -> preview shown to the writer as S4
	snippetIDByURL         map[string]string         // normalized URL -> "S4"
	snippetMap             map[string]int            // "S4" -> 1 (order of appearance)
	snippetCites           []SnippetRef
	droppedPhantomSnippets int
	retargetedSnippets     int
	tail                   string // the last part of the output, see rememberOutput

	links *linkResolver // nil: links are not checked
	// verify, when set, opens a link outside the session sources and says whether
	// it holds (deeploop.go); text is the link text, "" for a bare URL.
	verify             func(rawURL, text string) bool
	verifiedURLs       int
	droppedPhantomURLs int
	repairedURLs       int
	// codeRun is the length of the backtick run that opened the code the stream is
	// in (0: not in code). Addresses inside code are values to copy, not citations,
	// and are left alone (case 12, 29/10/2026).
	codeRun int

	lang                string // answer language of the deduction label ("" = English)
	deductions          int
	malformedDeductions int
}

// deductionWords are the tag spellings read as a deduction: the canonical
// "deduction" and the variants a model writes in the answer language or by
// mistake. Better a label too many than a bare deduction.
var deductionWords = []string{"deduction", "deductions", "deduzione", "deduktion", "schlussfolgerung",
	"déduction", "deduccion", "deducción", "inference", "inferenza"}

// SnippetRef maps a rendered snippet citation [Sn] to the search preview it stands for.
type SnippetRef struct {
	N         int    `json:"n"`
	SnippetID string `json:"snippet_id"`
	URL       string `json:"url"`
	Title     string `json:"title,omitempty"`
	Content   string `json:"-"`
}

// Lookahead limits: a construct still open after this many bytes is plain text.
const (
	maxTagLen         = 40  // [[fact:F12]], [[snip:S3]]
	maxBracketLen     = 400 // [text] or [url]
	maxMarkdownLinkLn = 1000
	maxBareURLLen     = 600
)

func NewReconciler(store *FactStore) *Reconciler {
	return &Reconciler{
		store:       store,
		citationMap: make(map[string]int),
		citations:   make([]FactCard, 0),
	}
}

// WithLanguage sets the language of the deduction label.
func (r *Reconciler) WithLanguage(lang string) *Reconciler {
	r.lang = lang
	return r
}

// WithSourceURLs enables link checking: links to these URLs are kept (with the
// source's exact spelling), links to anything else are removed and counted.
func (r *Reconciler) WithSourceURLs(urls []string) *Reconciler {
	r.links = newLinkResolver(urls)
	return r
}

// WithLinkVerifier lets a link outside the session sources be checked before it
// is dropped: a link that verify accepts is kept and becomes a known source.
func (r *Reconciler) WithLinkVerifier(verify func(rawURL, text string) bool) *Reconciler {
	r.verify = verify
	return r
}

// Process handles incoming token text, converting completed tags into citations
// and buffering partial constructs across chunk boundaries.
func (r *Reconciler) Process(chunk string) string {
	r.buffer += chunk
	return r.drain(false)
}

// Flush emits any remaining lookahead buffer at the end of the stream.
// If an unclosed citation tag (e.g. "[[fact:F1") remains in the buffer,
// it is dropped rather than printed literally, and registered as a dropped phantom.
func (r *Reconciler) Flush() string {
	return r.drain(true)
}

// drain emits everything in the buffer that can be decided now. When final is
// false, a construct that may still be completed by the next chunk stays buffered.
func (r *Reconciler) drain(final bool) string {
	var out strings.Builder
	for {
		i := r.nextConstruct(r.buffer)
		if i == -1 {
			hold := 0
			if !final {
				hold = r.partialSuffixLen(r.buffer)
			}
			r.emit(&out, r.buffer[:len(r.buffer)-hold])
			r.buffer = r.buffer[len(r.buffer)-hold:]
			return out.String()
		}
		r.emit(&out, r.buffer[:i])
		r.buffer = r.buffer[i:]

		consumed, text, wait := r.handle(r.buffer, final)
		if wait {
			return out.String()
		}
		r.emit(&out, text)
		r.buffer = r.buffer[consumed:]
	}
}

func (r *Reconciler) emit(out *strings.Builder, text string) {
	if text != "" {
		out.WriteString(text)
		r.lastOut = text[len(text)-1]
		r.rememberOutput(text)
	}
}

// nextConstruct is the index of the next byte that may start a tag, a link or
// an "about" tilde.
func (r *Reconciler) nextConstruct(s string) int {
	next := -1
	earliest := func(i int) {
		if i != -1 && (next == -1 || i < next) {
			next = i
		}
	}
	earliest(strings.IndexByte(s, '~'))
	earliest(strings.Index(s, notesMarkerStart))
	if r.links == nil {
		earliest(strings.Index(s, "[["))
		return next
	}
	earliest(strings.IndexByte(s, '`'))
	if r.codeRun > 0 {
		if r.codeRun < 3 {
			earliest(strings.IndexByte(s, '\n'))
		}
		return next
	}
	earliest(strings.IndexByte(s, '['))
	earliest(strings.Index(s, "http"))
	return next
}

// partialSuffixLen is how many trailing bytes may be the start of a construct.
func (r *Reconciler) partialSuffixLen(s string) int {
	prefixes := []string{"[[fact:", "[[snip:", notesMarkerStart}
	if r.links != nil {
		prefixes = []string{"https://", "http://", notesMarkerStart}
	}
	best := 0
	for _, p := range prefixes {
		for i := len(p) - 1; i > best; i-- {
			if strings.HasSuffix(s, p[:i]) {
				best = i
				break
			}
		}
	}
	return best
}

// handle decides the construct at the start of buf. It returns how many bytes
// were consumed and the text to emit for them, or wait=true when more input is
// needed to decide.
func (r *Reconciler) handle(buf string, final bool) (consumed int, text string, wait bool) {
	if strings.HasPrefix(buf, notesMarkerStart) {
		return r.handleNotesMarker(buf, final)
	}
	if r.links != nil && (buf[0] == '`' || r.codeRun > 0) {
		return r.handleCode(buf, final)
	}
	if buf[0] == '~' {
		return r.handleTilde(buf, final)
	}
	if strings.HasPrefix(buf, "http") {
		return r.handleBareURL(buf, final)
	}
	if strings.HasPrefix(buf, "[[") {
		if consumed, text, wait, ok := r.handleDeduction(buf, final); ok {
			return consumed, text, wait
		}
		for _, p := range []string{"[[fact:", "[[snip:"} {
			if strings.HasPrefix(buf, p) || (len(buf) < len(p) && strings.HasPrefix(p, buf)) {
				return r.handleTag(buf, final)
			}
		}
		if r.links != nil && isHTTPPrefix(buf[2:]) {
			// [[https://...]]: a bracketed URL with doubled brackets.
			end := strings.Index(buf, "]]")
			if end == -1 {
				if !final && len(buf) < maxBracketLen && !strings.Contains(buf, "\n") {
					return 0, "", true
				}
				return 1, "[", false
			}
			return end + 2, r.bracketedURL(buf[2:end]), false
		}
		return 1, "[", false
	}
	if buf[0] == '[' && r.links != nil {
		return r.handleBracket(buf, final)
	}
	return 1, buf[:1], false
}

// notesMarkerStart is enough of NotesMarker to look at what follows.
const notesMarkerStart = "<!--"

// handleNotesMarker: the mark where Go's notes begin, written by the model (a page may
// hold it, Vedetta's own code does), would hide the rest of the answer among the
// notes. It goes out spelled so that nothing takes it for the mark.
func (r *Reconciler) handleNotesMarker(buf string, final bool) (int, string, bool) {
	switch {
	case strings.HasPrefix(buf, NotesMarker):
		return len(NotesMarker), strings.Replace(NotesMarker, ":", "-", 1), false
	case !final && strings.HasPrefix(NotesMarker, buf):
		return 0, "", true
	}
	return len(notesMarkerStart), notesMarkerStart, false
}

// handleTilde turns the writer's "~" for "about" into "≈" (V25-F0.1): GFM
// renders text between two single tildes as strikethrough. Only a tilde that
// starts a word and comes before a digit, a currency sign or a capital letter
// ("~3 °C", "(~15%)", "~Nov 2027") changes; tildes inside a word or a URL
// ("…;199~art20"), paths ("~/.config") and "~~strikethrough~~" stay.
func (r *Reconciler) handleTilde(buf string, final bool) (int, string, bool) {
	if strings.HasPrefix(buf, "~~") {
		return 2, "~~", false
	}
	if len(buf) == 1 {
		if !final {
			return 0, "", true
		}
		return 1, "~", false
	}
	next, size := utf8.DecodeRuneInString(buf[1:])
	if next == utf8.RuneError && size <= 1 && !final && !utf8.FullRuneInString(buf[1:]) {
		return 0, "", true
	}
	switch r.lastOut {
	case 0, ' ', '\n', '\t', '(', '[', '|', ':':
	default:
		return 1, "~", false
	}
	if unicode.IsDigit(next) || unicode.IsUpper(next) || strings.ContainsRune("$€£", next) {
		return 1, "≈", false
	}
	return 1, "~", false
}

// handleTag resolves [[fact:ID]] and [[snip:ID]].
func (r *Reconciler) handleTag(buf string, final bool) (int, string, bool) {
	if !strings.HasPrefix(buf, "[[fact:") && !strings.HasPrefix(buf, "[[snip:") {
		// Only a prefix such as "[[f" so far.
		if final {
			return 1, "[", false
		}
		return 0, "", true
	}
	end := strings.Index(buf, "]]")
	if end == -1 {
		if final {
			r.droppedPhantoms++
			log.Printf("[reconciler] dropped unclosed citation tag in flush: %q", buf)
			return len(buf), "", false
		}
		if len(buf) > maxTagLen {
			// False alarm or unclosed tag: emit the opening literally and go on.
			return 1, "[", false
		}
		return 0, "", true
	}

	rawTag := buf[:end+2]
	kind := buf[2:6] // "fact" or "snip"
	id := strings.TrimSpace(buf[len("[[fact:"):end])
	if kind == "snip" {
		return end + 2, r.snippetCitation(rawTag, id), false
	}

	card, exists := r.store.Get(id)
	if !exists {
		r.droppedPhantoms++
		log.Printf("[reconciler] dropped phantom citation tag %q (session dropped: %d)", rawTag, r.droppedPhantoms)
		return end + 2, "", false
	}
	return end + 2, r.factCitation(card), false
}

// factCitation maps a fact to its rendered [n].
func (r *Reconciler) factCitation(card FactCard) string {
	citNum, alreadyCited := r.citationMap[card.ID]
	if !alreadyCited {
		citNum = len(r.citations) + 1
		r.citationMap[card.ID] = citNum
		r.citations = append(r.citations, card)
	}
	return fmt.Sprintf("[%d]", citNum)
}

// handleDeduction turns [[deduction]] (or a recognizable misspelling) into the
// localized label. ok is false when buf is not a deduction tag.
func (r *Reconciler) handleDeduction(buf string, final bool) (consumed int, text string, wait, ok bool) {
	end := strings.Index(buf, "]]")
	if end == -1 || end > maxTagLen {
		inner := buf[2:]
		if !final && len(buf) < maxTagLen && !strings.Contains(inner, "\n") && inner != "" && isDeductionPrefix(inner) {
			return 0, "", true, true
		}
		return 0, "", false, false
	}
	raw := buf[2:end]
	word := strings.ToLower(strings.TrimRight(strings.TrimSpace(raw), ":"))
	if !slices.Contains(deductionWords, word) {
		return 0, "", false, false
	}
	rest := buf[end+2:]
	if rest == "" && !final {
		return 0, "", true, true // the next byte tells whether a space is needed
	}
	r.deductions++
	if raw != "deduction" {
		r.malformedDeductions++
		log.Printf("[reconciler] deduction tag spelled %q, labelled anyway", buf[:end+2])
	}
	label := StepMessage(r.lang, "deduction_label")
	if rest == "" || (rest[0] != ' ' && rest[0] != '\n') {
		label += " "
	}
	return end + 2, label, false, true
}

// isDeductionPrefix reports whether s may still grow into a deduction tag.
func isDeductionPrefix(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return false
	}
	for _, w := range deductionWords {
		if strings.HasPrefix(w, s) || strings.HasPrefix(s, w) {
			return true
		}
	}
	return false
}

// snippetCitation maps a snippet ID shown to the writer to its rendered [Sn].
func (r *Reconciler) snippetCitation(rawTag, id string) string {
	id = strings.ToUpper(id)
	if id != "" && id[0] != 'S' {
		id = "S" + id
	}
	snip, ok := r.snippets[id]
	if !ok {
		r.droppedPhantomSnippets++
		log.Printf("[reconciler] dropped phantom snippet tag %q (session dropped: %d)", rawTag, r.droppedPhantomSnippets)
		return ""
	}
	sentence := currentSentence(r.tail)
	if card, better := betterFactFor(sentence, snip, r.store.All(), r.lang); better {
		r.retargetedSnippets++
		citation := r.factCitation(card)
		if strings.Contains(sentence, citation) {
			return "" // the sentence already cites it
		}
		return citation
	}
	n, cited := r.snippetMap[id]
	if !cited {
		n = len(r.snippetCites) + 1
		r.snippetMap[id] = n
		r.snippetCites = append(r.snippetCites, SnippetRef{N: n, SnippetID: id, URL: snip.URL, Title: snip.Title, Content: snip.Content})
	}
	return fmt.Sprintf("[S%d]", n)
}

// handleBracket resolves [url] and markdown links [text](url); any other bracket
// is plain text.
func (r *Reconciler) handleBracket(buf string, final bool) (int, string, bool) {
	limit := min(len(buf), maxBracketLen)
	closeIdx := strings.IndexByte(buf[:limit], ']')
	nl := strings.IndexByte(buf[:limit], '\n')
	if closeIdx == -1 || (nl != -1 && nl < closeIdx) {
		if !final && nl == -1 && len(buf) < maxBracketLen {
			return 0, "", true
		}
		return 1, "[", false
	}
	inner := buf[1:closeIdx]

	if closeIdx+1 == len(buf) && !final {
		return 0, "", true // the next byte tells whether this is a markdown link
	}
	if closeIdx+1 < len(buf) && buf[closeIdx+1] == '(' {
		rest := buf[closeIdx+2:]
		lim := min(len(rest), maxMarkdownLinkLn)
		nlP := strings.IndexAny(rest[:lim], " \n")
		closeP := closingParen(rest[:lim])
		if closeP == -1 && (final || nlP != -1 || lim == maxMarkdownLinkLn) {
			closeP = strings.IndexByte(rest[:lim], ')') // no parenthesis closes the link's own: the first one does
		}
		if closeP == -1 || (nlP != -1 && nlP < closeP) {
			if !final && nlP == -1 && len(buf) < maxMarkdownLinkLn {
				return 0, "", true
			}
			return 1, "[", false
		}
		target := rest[:closeP]
		consumed := closeIdx + 2 + closeP + 1
		if !isHTTPPrefix(target) {
			return consumed, buf[:consumed], false
		}
		return consumed, r.markdownLink(inner, target), false
	}
	if isHTTPPrefix(inner) {
		return closeIdx + 1, r.bracketedURL(inner), false
	}
	return 1, "[", false
}

// markdownLink keeps [text](url) when url is a session source (spelled as the
// source), otherwise keeps only the text.
func (r *Reconciler) markdownLink(text, target string) string {
	if localAddress(target) {
		if isHTTPPrefix(text) || strings.TrimSpace(text) == "" {
			return "`" + target + "`"
		}
		return text + " (`" + target + "`)"
	}
	resolved, ok := r.resolveLink(target, text)
	if isHTTPPrefix(text) {
		if !ok {
			return r.unverifiedLabel()
		}
		if t, tok := r.resolve(text); tok {
			text = t
		}
	}
	if !ok {
		if looksLikeURL(text) {
			// The text of a dropped link is itself an address (github.com/x/y): showing it
			// would pass an unchecked address on as plain text (case 8, 29/10/2026).
			return r.unverifiedLabel()
		}
		return text
	}
	return "[" + text + "](" + resolved + ")"
}

var urlLikeRe = regexp.MustCompile(`^(?:www\.)?[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+/\S*$|^www\.\S+$`)

// looksLikeURL reports whether a link text is an address without the scheme, such
// as github.com/owner/repo or www.site.it; a bare name like Node.js is not.
func looksLikeURL(text string) bool {
	return urlLikeRe.MatchString(strings.TrimSpace(text))
}

// bracketedURL turns [url] into a snippet citation [Sn] when it is a preview the
// writer was shown, keeps it when it is another session source, and drops it
// otherwise.
func (r *Reconciler) bracketedURL(raw string) string {
	if localAddress(raw) {
		return "`" + raw + "`"
	}
	resolved, ok := r.resolve(raw)
	if !ok {
		return ""
	}
	if id, isSnippet := r.snippetIDByURL[normalizeLinkURL(resolved)]; isSnippet {
		return r.snippetCitation("["+raw+"]", id)
	}
	return "[" + resolved + "]"
}

// handleCode follows inline code and fenced blocks: a backtick run opens code,
// the same run closes it, and inline code also ends at a newline so that a stray
// backtick does not exempt the rest of the answer from the link check.
func (r *Reconciler) handleCode(buf string, final bool) (int, string, bool) {
	if buf[0] != '`' {
		if buf[0] == '\n' && r.codeRun < 3 {
			r.codeRun = 0
		}
		return 1, buf[:1], false
	}
	n := 0
	for n < len(buf) && buf[n] == '`' {
		n++
	}
	if n == len(buf) && !final {
		return 0, "", true
	}
	switch {
	case r.codeRun == 0:
		r.codeRun = n
	case n == r.codeRun:
		r.codeRun = 0
	}
	return n, buf[:n], false
}

// closingParen is the index of the parenthesis that closes a link, past the ones that
// open and close inside its address (Wikipedia's), or -1. It stops at a space.
func closingParen(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\n':
			return -1
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

// bareURLEnd is where a URL written as plain text ends, or -1 when it may go on. A
// parenthesis that closes one opened inside the address is part of it.
func bareURLEnd(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', ']', '>', '"', '\'', '<', '`':
			return i
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

// handleBareURL checks a URL written as plain text.
func (r *Reconciler) handleBareURL(buf string, final bool) (int, string, bool) {
	if !strings.HasPrefix(buf, "http://") && !strings.HasPrefix(buf, "https://") {
		if !final && (strings.HasPrefix("https://", buf) || strings.HasPrefix("http://", buf)) {
			return 0, "", true
		}
		return 1, buf[:1], false
	}
	end := bareURLEnd(buf)
	if end == -1 {
		if !final && len(buf) < maxBareURLLen {
			return 0, "", true
		}
		end = len(buf)
	}
	raw := strings.TrimRight(buf[:end], ".,;:!?*") // the asterisks of **http://…** are not the address
	if localAddress(raw) {
		return len(raw), "`" + raw + "`", false
	}
	resolved, ok := r.resolve(raw)
	if !ok {
		return len(raw), r.unverifiedLabel(), false
	}
	return len(raw), resolved, false
}

// localAddress reports an address on the reader's own machine or network, such as
// http://localhost:3000, http://ollama:11434 or http://192.168.1.10: something to
// type, not a source to check. It stays as code, so that a guide still says where to
// go and a page cannot make it a link (series of 05/11/2026: "Poi apri *link non
// verificato*").
func localAddress(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast()
	}
	if !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// unverifiedLabel takes the place of an address that was dropped.
func (r *Reconciler) unverifiedLabel() string {
	return "*" + StepMessage(r.lang, "link_unverified") + "*"
}

// resolve checks one URL against the session sources and keeps the counters.
func (r *Reconciler) resolve(raw string) (string, bool) {
	return r.resolveLink(raw, "")
}

// resolveLink is resolve with the link text, which the verifier may use.
func (r *Reconciler) resolveLink(raw, text string) (string, bool) {
	resolved, repaired, ok := r.links.Resolve(raw)
	if !ok && r.verify != nil && r.links != nil && r.verify(raw, text) {
		r.verifiedURLs++
		r.links.add(raw)
		log.Printf("[reconciler] link outside the session sources opened and verified: %q (session verified: %d)", raw, r.verifiedURLs)
		return raw, true
	}
	switch {
	case !ok:
		r.droppedPhantomURLs++
		log.Printf("[reconciler] dropped link outside the session sources: %q (session dropped: %d)", raw, r.droppedPhantomURLs)
	case repaired:
		r.repairedURLs++
		log.Printf("[reconciler] repaired link %q -> %q", raw, resolved)
	}
	return resolved, ok
}

func isHTTPPrefix(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// DroppedPhantomURLs returns how many links outside the session sources were removed.
func (r *Reconciler) DroppedPhantomURLs() int {
	return r.droppedPhantomURLs
}

// RepairedURLs returns how many links were rewritten to a source's exact URL.
// VerifiedURLs counts links outside the session sources that the verifier accepted.
func (r *Reconciler) VerifiedURLs() int {
	return r.verifiedURLs
}

func (r *Reconciler) RepairedURLs() int {
	return r.repairedURLs
}

// Deductions returns how many deduction tags were turned into a label.
func (r *Reconciler) Deductions() int {
	return r.deductions
}
