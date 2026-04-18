package translate

import (
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

func IsEnglish(text string) bool {
	code := DetectLanguage(text)
	return code == "en"
}

// DetectThreadLanguage examines the last few message texts and returns the
// dominant non-English language code plus whether the detection is confident.
// English messages are counted so that a mostly-English thread is not
// mistakenly flagged as non-English.
func DetectThreadLanguage(texts []string) (string, bool) {
	counts := make(map[string]int)
	examined := 0
	limit := 10
	for i := len(texts) - 1; i >= 0 && examined < limit; i-- {
		code := DetectLanguage(texts[i])
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
	enCount := counts["en"]
	confident := bestCount >= 3 && bestCount > enCount
	return best, confident
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
	base := dialect
	if idx := strings.IndexByte(dialect, '-'); idx >= 0 {
		base = dialect[:idx]
	}
	if isoCode == base {
		return dialect
	}
	return isoCode
}
