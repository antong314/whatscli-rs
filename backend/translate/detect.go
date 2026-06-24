package translate

import (
	"net/url"
	"strings"
	"unicode"

	"github.com/pemistahl/lingua-go"
)

var detector lingua.LanguageDetector

func init() {
	detector = lingua.NewLanguageDetectorBuilder().
		FromAllLanguages().
		WithMinimumRelativeDistance(0.05).
		WithPreloadedLanguageModels().
		Build()
}

// DetectLanguage returns the most likely ISO-639-1 language code for the given
// text using lingua, or "" if no language could be determined. Kept for
// backwards compatibility with callers that just want a single label.
func DetectLanguage(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	lang, exists := detector.DetectLanguageOf(text)
	if !exists {
		return ""
	}
	return linguaToISO(lang)
}

// IsEnglish reports whether `text` is most likely English. Used as a hint by
// callers; not a hard veto on translation (the per-message classifier in
// ClassifyMessage already accounts for English-confidence properly).
func IsEnglish(text string) bool {
	return DetectLanguage(text) == "en"
}

// DetectThreadLanguage examines the most recent ~30 incoming messages in
// a thread and returns the dominant non-English language plus a
// `confident` flag.
//
// The `confident` flag is the signal the rest of the system uses to
// decide whether to *act* on the thread language - the inbound classifier
// uses it as a soft fallback for ambiguous messages, the outbound
// translator uses it as the basic "should we even consider translating
// this send?" gate. Bias here directly determines how often we get
// outbound translation wrong, so we are deliberately conservative:
//
//  1. A thread is "confidently non-English" only when at least
//     fractionalThreshold of the examined non-trivial messages tally to
//     a single non-English language, AND that language has at least
//     minAbsoluteNonEnglish messages in absolute terms. Both bars exist
//     because thresholds based on either alone fail in obvious ways
//     (a 2-message-window thread doesn't tell us anything; a single
//     non-English greeting in a 30-message English thread doesn't
//     either).
//
//  2. The per-message language identifier is more careful than just
//     "did 'hola' appear?". See identifyMessageLanguage - the strong-
//     signal path only fires for *short* messages where lingua is
//     unreliable; longer messages go through lingua, which correctly
//     classifies "Hola hola, can you pick her up at 7?" as English.
//     Without this, a single mostly-English message that happened to
//     start with a Spanish greeting word would be miscounted as
//     Spanish and drag the thread tally with it.
func DetectThreadLanguage(texts []string) (string, bool) {
	const window = 30
	const minAbsoluteNonEnglish = 4
	const fractionalThreshold = 0.4

	counts := make(map[string]int)
	examined := 0
	for i := len(texts) - 1; i >= 0 && examined < window; i-- {
		text := texts[i]
		if isTrivialNoOp(text) {
			continue
		}
		code := identifyMessageLanguage(text)
		if code == "" {
			continue
		}
		counts[code]++
		examined++
	}
	if examined == 0 {
		return "", false
	}

	best := ""
	bestCount := 0
	for code, count := range counts {
		if code == "en" {
			continue
		}
		if count > bestCount {
			best = code
			bestCount = count
		}
	}
	if best == "" {
		return "", false
	}

	confident := bestCount >= minAbsoluteNonEnglish &&
		float64(bestCount) >= fractionalThreshold*float64(examined)
	return best, confident
}

// IdentifyMessageLanguage is the exported alias for
// identifyMessageLanguage. Used by callers outside the package that
// want the same "be careful on short text, trust lingua on long text"
// per-message classification the thread tally uses (e.g. the outbound
// auto-translate gate, when computing how non-English the recent thread
// actually is).
func IdentifyMessageLanguage(text string) string { return identifyMessageLanguage(text) }

// identifyMessageLanguage returns the best ISO-639-1 code for `text`,
// returning "" if no language can be identified. It is more careful than
// raw DetectLanguage on the short / mixed-content messages typical of
// WhatsApp, while deferring to lingua for anything substantial:
//
//   - For short text (< shortTextWordLimit words after tokenization),
//     the strong-signal heuristics fire first - lingua is unreliable
//     here, but "ñ" or "Hola" or "Gracias" alone are nearly definitive.
//
//   - For longer text, we trust lingua. Crucially this means a message
//     like "Hola hola, Is 7h30 good for you or you would prefer now?"
//     is correctly classified as English (lingua sees the seven English
//     content words and calls it English) instead of being misclassified
//     as Spanish because it happens to *contain* "hola". Without this
//     fix, threads where the counterpart sometimes starts a message with
//     a foreign-language greeting would falsely flip to that language.
//
// Used by DetectThreadLanguage for the thread tally. The per-message
// translation classifier (ClassifyMessage) uses a slightly different
// flow because it has more context (an explicit threadLang prior) and
// different cost trade-offs.
func identifyMessageLanguage(text string) string {
	wordCount := len(tokenize(strings.ToLower(text)))
	const shortTextWordLimit = 4

	if wordCount <= shortTextWordLimit {
		if signal := strongLanguageSignal(text); signal != "" {
			return signal
		}
	}
	return DetectLanguage(text)
}

// TranslationDecision is the output of ClassifyMessage.
type TranslationDecision int

const (
	// DecisionSkip means we should not translate this message.
	DecisionSkip TranslationDecision = iota
	// DecisionTranslate means we should translate using the returned
	// source language.
	DecisionTranslate
)

// classifierThresholds are the confidence cut-offs used by ClassifyMessage.
// Pulled out as constants (not a struct) so they're searchable and easy to
// tune without restructuring the call sites.
//
// Lingua's *absolute* confidence values come out very low for the short,
// casual text that dominates WhatsApp - "We're leaving" scores English
// at 0.293, and a clearly Spanish "Como vas viendo el trabajo?" scores
// Spanish at only 0.540. Any threshold based on absolute confidence
// (>=0.85, say) would miss almost everything.
//
// What *is* reliable, even on short text, is the relative gap between
// the top language and the runner-up. So we use **ratio-based margins**:
// "top is at least this many times bigger than the next contender".
// These work on the actual range of values lingua produces on chat-like
// strings.
const (
	// A message is "confidently non-English" if the top non-English
	// language has at least this much absolute mass *and* dominates
	// English by this ratio. Using a ratio rather than a margin makes
	// the rule scale-invariant: it works equally well on long English
	// text (ratio ~big against tiny Spanish noise) and on short Spanish
	// (ratio ~big against tiny English noise).
	nonEnglishMinAbsolute = 0.10
	nonEnglishOverEnglish = 5.0

	// A message is "confidently English" if English is the top language
	// and clears the runner-up by this ratio. This is symmetric to the
	// non-English rule. Pegged a bit lower (3x) than the non-English
	// case (5x) because English-vs-noise tends to be cleaner than
	// Spanish-vs-Esperanto/Portuguese/etc.
	englishOverRunnerUp = 3.0

	// Soft bar when the per-message top language matches the thread
	// prior. We don't need confidence in the message alone - the thread
	// already agrees - so a modest absolute score is enough to act.
	confidenceWithThreadAgreement = 0.20
)

// ClassifyMessage decides whether to translate `text`, and if so, what
// source language to use. `threadLang` is the dominant non-English language
// of the surrounding chat (or ""), as returned by DetectThreadLanguage.
//
// Decision rules, in order:
//
//  1. Trivially non-linguistic content ("ok", emoji-only, URL-only,
//     numbers/punctuation only) is skipped.
//
//  2. A "strong non-English signal" - distinctive accented characters,
//     or one of a small set of high-frequency function words from a
//     known language - forces translation, regardless of the thread.
//     This is the cheapest way to catch short messages lingua misses:
//     "Hola", "Gracias", "Como estas", "Mañana voy".
//
//  3. Compute lingua confidence values once. If a non-English language
//     dominates English by a wide ratio, translate.
//
//  4. If the message-level top non-English language matches the thread
//     prior with a softer absolute threshold, translate.
//
//  5. If English dominates the runner-up by a wide ratio, skip.
//
//  6. Ambiguous: trust the thread. If the thread prior is non-English,
//     use it as the source language and translate; otherwise skip.
func ClassifyMessage(text, threadLang string) (string, TranslationDecision) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", DecisionSkip
	}
	if isTrivialNoOp(text) {
		return "", DecisionSkip
	}

	// (2) Strong, deterministic signals first - cheap and catches cases
	// lingua misses on short text.
	if signal := strongLanguageSignal(text); signal != "" {
		return signal, DecisionTranslate
	}

	values := detector.ComputeLanguageConfidenceValues(text)
	enScore, topLang, topScore, runnerUpScore := splitConfidence(values)

	// (3) Confident non-English by message alone (ratio-based).
	if topLang != "" && topLang != "en" &&
		topScore >= nonEnglishMinAbsolute &&
		topScore >= nonEnglishOverEnglish*safeFloor(enScore) {
		return topLang, DecisionTranslate
	}

	// (4) Softer per-message signal that agrees with the thread prior.
	if topLang != "" && topLang != "en" &&
		topLang == threadLang &&
		topScore >= confidenceWithThreadAgreement {
		return topLang, DecisionTranslate
	}

	// (5) Confident English (ratio-based).
	if topLang != "en" && enScore >= englishOverRunnerUp*safeFloor(topScore) ||
		topLang == "en" && topScore >= englishOverRunnerUp*safeFloor(runnerUpScore) {
		// Note: lingua's confidence values list is sorted by score, so
		// `topLang == "en"` means English is #1 and `runnerUpScore` is
		// the next best score; otherwise English is somewhere below
		// `topScore` (the best non-English) and we compare the two
		// directly. Both branches answer the same question: "is
		// English clearly ahead of the next contender?"
		return "", DecisionSkip
	}

	// (6) Ambiguous - fall back to the thread's prior.
	if threadLang != "" && threadLang != "en" {
		return threadLang, DecisionTranslate
	}
	return "", DecisionSkip
}

// safeFloor clamps a denominator to a tiny epsilon so ratio comparisons
// don't blow up when lingua reports a near-zero score for a language. We
// use 0.01 (1% absolute) - any signal weaker than that should not matter
// for our ratio thresholds.
func safeFloor(v float64) float64 {
	const eps = 0.01
	if v < eps {
		return eps
	}
	return v
}

// splitConfidence extracts the bits of a lingua confidence-values slice
// that ClassifyMessage actually needs:
//   - enScore        : confidence assigned to English (0 if not in list)
//   - topLang        : ISO code of the best non-English language
//   - topScore       : its confidence
//   - runnerUpScore  : the second-best score across the *entire* slice
//     (i.e. the language directly below #1, English or not)
//
// Values come back sorted by score descending, so we just walk once.
func splitConfidence(values []lingua.ConfidenceValue) (enScore float64, topLang string, topScore float64, runnerUpScore float64) {
	for i, v := range values {
		code := linguaToISO(v.Language())
		if i == 1 {
			runnerUpScore = v.Value()
		}
		if code == "en" {
			enScore = v.Value()
		} else if topLang == "" {
			topLang = code
			topScore = v.Value()
		}
	}
	return enScore, topLang, topScore, runnerUpScore
}

// IsTrivialNoOp is the exported alias for isTrivialNoOp; callers outside
// the package use this when they need the same "is this content even
// worth a translation decision?" check (e.g. the outbound auto-translate
// gate, which wants to look only at substantive recent messages).
func IsTrivialNoOp(text string) bool { return isTrivialNoOp(text) }

// isTrivialNoOp reports whether text has no meaningful linguistic content
// to translate (single-emoji acks, bare URLs, numbers-only, single-word
// stock acks). These should never trigger the LLM.
func isTrivialNoOp(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return true
	}

	// All characters non-letter (emoji-only, punctuation-only, all digits).
	hasLetter := false
	for _, r := range t {
		if unicode.IsLetter(r) {
			hasLetter = true
			break
		}
	}
	if !hasLetter {
		return true
	}

	// Bare URL.
	if isBareURL(t) {
		return true
	}

	// Stock English / Spanish acks. We list the common ones in both
	// languages so that an "Ok" reply in a Spanish thread doesn't trip
	// the thread-fallback into translating the literal string "Ok".
	//
	// We compare against a normalized form: lowercased, with trailing
	// punctuation/whitespace stripped. So "Thank you." and "thanks!!"
	// match the same way "thanks" does. We deliberately do *not* strip
	// internal whitespace - "thank you" stays a 2-word phrase.
	normalized := strings.TrimRight(strings.ToLower(t), " .!?,")
	switch normalized {
	case "ok", "okay", "k", "kk", "yes", "no", "y", "n", "lol", "haha",
		"thanks", "thx", "ty", "thank you", "thank u",
		"si", "sí", "vale", "okok", "ok ok",
		"gracias", "de nada":
		// Note: "gracias" is in here as a trivial ack BUT
		// strongLanguageSignal also flags it as Spanish; the strong-
		// signal path runs after isTrivialNoOp in ClassifyMessage, so
		// "gracias" alone is correctly skipped. A longer message
		// containing "gracias" is not trivial and reaches the
		// strong-signal check.
		return true
	}

	return false
}

func isBareURL(text string) bool {
	t := strings.TrimSpace(text)
	if strings.ContainsAny(t, " \t\n") {
		return false
	}
	if !strings.HasPrefix(t, "http://") && !strings.HasPrefix(t, "https://") {
		return false
	}
	if _, err := url.Parse(t); err != nil {
		return false
	}
	return true
}

// strongLanguageSignal returns an ISO-639-1 code if `text` contains a
// near-unambiguous indicator of a particular language, or "" otherwise.
//
// We use two cheap signal classes:
//
//  1. Diacritics that are essentially exclusive to one language family.
//     The Spanish-only ones (ñ/¿/¡) are the highest-precision signal we
//     have for short text; the rest (á/é/í/ó/ú/ü) appear in multiple
//     Romance languages but in our user's case (Spanish-default dialect)
//     biasing toward Spanish is right ~all the time, and the LLM can
//     handle a wrong-source-language hint anyway.
//
//  2. A small allowlist of high-frequency function words / greetings
//     that are very unlikely to appear in natural English text. This
//     catches "Hola", "Gracias", "Como estas", "Mañana voy por el carro"
//     - the exact short messages that lingua misclassifies as English.
//
// Order matters: we check the unambiguous diacritic signals first, then
// the word allowlist.
func strongLanguageSignal(text string) string {
	if hasSpanishDiacritic(text) {
		return "es"
	}

	// Word-level signal. Lowercase + split on non-letter to avoid being
	// confused by punctuation. We only need to find ONE distinctive word
	// to call it.
	lower := strings.ToLower(text)
	for word := range tokenize(lower) {
		if lang, ok := strongWordSignal[word]; ok {
			return lang
		}
	}
	return ""
}

// hasSpanishDiacritic reports whether the text contains a character that
// is highly characteristic of Spanish: ñ, ¿, ¡. á/é/í/ó/ú/ü are also
// Spanish but they appear in other Romance languages too, so we don't
// treat them as a unilateral Spanish signal here - lingua can disambiguate
// those via the confidence-values path.
func hasSpanishDiacritic(text string) bool {
	for _, r := range text {
		switch r {
		case 'ñ', 'Ñ', '¿', '¡':
			return true
		}
	}
	return false
}

// tokenize splits text on non-letter runs and yields lowercase words.
// Returned as a set (map[string]struct{}) so callers can do O(1) checks.
func tokenize(text string) map[string]struct{} {
	out := make(map[string]struct{})
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out[b.String()] = struct{}{}
			b.Reset()
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// strongWordSignal maps high-confidence function words and greetings to
// their language code. Keep this list short and high-precision: a word
// belongs here only if seeing it in a message makes the language
// near-certain. Conservative bias on purpose - a missed signal falls
// through to lingua and the thread fallback, but a wrong signal forces
// the LLM to translate text that doesn't need it.
//
// All entries are lowercase; tokenize() lowercases input.
var strongWordSignal = map[string]string{
	// Spanish - the dominant target for our user.
	"hola":     "es",
	"gracias":  "es",
	"mañana":   "es", // diacritic path catches this too; belt-and-suspenders.
	"buenos":   "es",
	"buenas":   "es",
	"días":     "es",
	"noches":   "es",
	"tardes":   "es",
	"claro":    "es",
	"vale":     "es",
	"hoy":      "es",
	"ayer":     "es",
	"esto":     "es",
	"eso":      "es",
	"está":     "es",
	"qué":      "es",
	"cómo":     "es",
	"viendo":   "es",
	"trabajo":  "es",
	"trabajar": "es",
	"hacer":    "es",
	"haciendo": "es",
	"puedo":    "es",
	"puede":    "es",
	"necesito": "es",
	"voy":      "es",
	"vamos":    "es",
	"para":     "es",
	"con":      "es",
	"pero":     "es",
	"porque":   "es",
	"también":  "es",
	"reunión":  "es",
	"hasta":    "es",

	// Portuguese - watch out for overlap with Spanish; we keep this
	// minimal and only include words that are PT-distinctive (i.e. do
	// NOT also appear in Spanish). "está" is shared with Spanish and is
	// already mapped above, so it stays out of this section.
	"obrigado": "pt",
	"obrigada": "pt",
	"você":     "pt",

	// French.
	"bonjour":     "fr",
	"merci":       "fr",
	"oui":         "fr",
	"aujourd'hui": "fr",

	// German.
	"danke":  "de",
	"hallo":  "de",
	"gestern": "de",
	"morgen":  "de", // German "tomorrow"; in EN it's a name. Risk acceptable.
}

func linguaToISO(lang lingua.Language) string {
	return strings.ToLower(lang.IsoCode639_1().String())
}

// dialectLanguageName returns a human-readable regional name for a dialect
// code. For generic codes it falls back to the base language name.
func dialectLanguageName(code string) string {
	if name, ok := dialectNames[code]; ok {
		return name
	}
	base := code
	if idx := strings.IndexByte(code, '-'); idx >= 0 {
		base = code[:idx]
	}
	if name, ok := baseLanguageNames[base]; ok {
		return name
	}
	if len(base) == 0 {
		return base
	}
	r := []rune(base)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

var dialectNames = map[string]string{
	"es-CR": "Costa Rican Spanish",
	"es-MX": "Mexican Spanish",
	"es-AR": "Argentine Spanish",
	"es-CO": "Colombian Spanish",
	"es-CL": "Chilean Spanish",
	"es-PE": "Peruvian Spanish",
	"es-VE": "Venezuelan Spanish",
	"es-EC": "Ecuadorian Spanish",
	"es-GT": "Guatemalan Spanish",
	"es-CU": "Cuban Spanish",
	"es-BO": "Bolivian Spanish",
	"es-DO": "Dominican Spanish",
	"es-HN": "Honduran Spanish",
	"es-PY": "Paraguayan Spanish",
	"es-SV": "Salvadoran Spanish",
	"es-NI": "Nicaraguan Spanish",
	"es-PA": "Panamanian Spanish",
	"es-UY": "Uruguayan Spanish",
	"es-PR": "Puerto Rican Spanish",
	"es-ES": "Castilian Spanish",
	"pt-BR": "Brazilian Portuguese",
	"pt-PT": "European Portuguese",
	"fr-CA": "Canadian French",
	"fr-FR": "Metropolitan French",
	"en-US": "American English",
	"en-GB": "British English",
	"en-AU": "Australian English",
}

var baseLanguageNames = map[string]string{
	"es": "Spanish",
	"en": "English",
	"fr": "French",
	"de": "German",
	"it": "Italian",
	"pt": "Portuguese",
	"ru": "Russian",
	"zh": "Chinese",
	"ja": "Japanese",
	"ko": "Korean",
	"ar": "Arabic",
	"hi": "Hindi",
	"nl": "Dutch",
	"pl": "Polish",
	"tr": "Turkish",
	"vi": "Vietnamese",
	"th": "Thai",
	"sv": "Swedish",
	"da": "Danish",
	"fi": "Finnish",
	"no": "Norwegian",
	"cs": "Czech",
	"el": "Greek",
	"he": "Hebrew",
	"hu": "Hungarian",
	"id": "Indonesian",
	"ms": "Malay",
	"ro": "Romanian",
	"sk": "Slovak",
	"uk": "Ukrainian",
	"bg": "Bulgarian",
	"hr": "Croatian",
	"sr": "Serbian",
	"sl": "Slovenian",
	"et": "Estonian",
	"lv": "Latvian",
	"lt": "Lithuanian",
	"tl": "Tagalog",
	"sw": "Swahili",
	"ca": "Catalan",
	"gl": "Galician",
	"eu": "Basque",
}

// ISOToDialectCode maps a generic ISO code (e.g. "es") to the configured
// dialect code (e.g. "es-CR") when the base language matches.
func ISOToDialectCode(isoCode, dialect string) string {
	base := BaseLanguageCode(dialect)
	if isoCode == base {
		return dialect
	}
	return isoCode
}

// BaseLanguageCode strips the regional suffix off a dialect code, e.g.
// "es-CR" -> "es", "en-US" -> "en", "es" -> "es", "" -> "".
func BaseLanguageCode(code string) string {
	if idx := strings.IndexByte(code, '-'); idx >= 0 {
		return code[:idx]
	}
	return code
}
