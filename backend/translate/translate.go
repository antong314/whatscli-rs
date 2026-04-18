package translate

import (
	"fmt"
	"strings"
	"sync"

	llama "github.com/tcpipuk/llama-go"
)

type Translator struct {
	model   *llama.Model
	ctx     *llama.Context
	mu      sync.Mutex
	dialect string
	ready   bool
}

// New creates a Translator. Call Init to load the model.
func New(dialect string) *Translator {
	if dialect == "" {
		dialect = "es-CR"
	}
	return &Translator{dialect: dialect}
}

// Init downloads the model if needed and loads it. The progress callback
// reports download progress (only called when a download is required).
func (t *Translator) Init(modelPath string, progress ProgressFunc) error {
	if modelPath == "" {
		modelPath = GetDefaultModelPath()
	}
	if !ModelExists(modelPath) {
		if err := DownloadModel(modelPath, progress); err != nil {
			return err
		}
	}
	model, err := loadModel(modelPath)
	if err != nil {
		return err
	}
	ctx, err := newContext(model)
	if err != nil {
		model.Close()
		return err
	}
	t.model = model
	t.ctx = ctx
	t.ready = true
	return nil
}

func (t *Translator) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ready = false
	if t.ctx != nil {
		t.ctx.Close()
		t.ctx = nil
	}
	if t.model != nil {
		t.model.Close()
		t.model = nil
	}
}

func (t *Translator) IsReady() bool {
	return t.ready
}

func (t *Translator) Dialect() string {
	return t.dialect
}

// Translate translates text from sourceLangCode to targetLangCode.
// Language codes should be ISO 639-1 or regional (e.g. "es", "es-CR", "en").
func (t *Translator) Translate(text, sourceLangCode, targetLangCode string) (string, error) {
	if !t.ready {
		return "", fmt.Errorf("translator not initialized")
	}

	prompt := buildPrompt(text, sourceLangCode, targetLangCode, t.dialect)

	t.mu.Lock()
	defer t.mu.Unlock()

	result, err := t.ctx.Generate(prompt, llama.WithMaxTokens(512))
	if err != nil {
		return "", fmt.Errorf("inference: %w", err)
	}

	return strings.TrimSpace(result), nil
}

// TranslateToEnglish is a convenience wrapper for translating to English.
func (t *Translator) TranslateToEnglish(text, sourceLangCode string) (string, error) {
	return t.Translate(text, sourceLangCode, "en")
}

// TranslateFromEnglish translates English text to the target language.
func (t *Translator) TranslateFromEnglish(text, targetLangCode string) (string, error) {
	return t.Translate(text, "en", targetLangCode)
}

func buildPrompt(text, srcCode, tgtCode, dialect string) string {
	srcCode = resolveDialect(srcCode, dialect)
	tgtCode = resolveDialect(tgtCode, dialect)

	srcName := dialectLanguageName(srcCode)
	tgtName := dialectLanguageName(tgtCode)

	var extra string
	if srcCode == "es-CR" || tgtCode == "es-CR" {
		if tgtCode == "es-CR" {
			extra = "Your goal is to produce natural Costa Rican Spanish, using local expressions, " +
				"vocabulary, and phrasing typical of Costa Rica rather than generic or Castilian Spanish.\n"
		} else {
			extra = "Your goal is to accurately convey the meaning and nuances of the original " +
				srcName + " text, preserving the intent of local expressions and slang " +
				"(e.g. 'mae', 'pura vida', 'tuanis', 'diay'), while producing natural " + tgtName + ".\n"
		}
	} else {
		extra = "Your goal is to accurately convey the meaning and nuances of the original " +
			srcName + " text while adhering to " + tgtName + " grammar, vocabulary, and cultural sensitivities.\n"
	}

	body := fmt.Sprintf(
		"You are a professional %s (%s) to %s (%s) translator.\n%s"+
			"Produce only the %s translation, without any additional explanations or commentary. "+
			"Please translate the following %s text into %s:\n\n\n%s",
		srcName, srcCode, tgtName, tgtCode,
		extra,
		tgtName, srcName, tgtName,
		strings.TrimSpace(text),
	)
	return "<start_of_turn>user\n" + body + "<end_of_turn>\n<start_of_turn>model\n"
}

// resolveDialect upgrades a bare ISO code to the configured dialect when the
// base language matches.
func resolveDialect(code, dialect string) string {
	if strings.IndexByte(code, '-') >= 0 {
		return code
	}
	return ISOToDialectCode(code, dialect)
}
