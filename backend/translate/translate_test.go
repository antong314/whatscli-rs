package translate

import (
	"strings"
	"testing"
)

func TestCleanTranslationOutput_DropsAlternativesForSingleLineSource(t *testing.T) {
	const generated = "Cuando te sea posible.\n" +
		"Cuando tengas tiempo.\n" +
		"Cuando estés disponible.\n" +
		"Cuando te venga bien."

	if got, want := cleanTranslationOutput("whenever you are available", generated), "Cuando te sea posible."; got != want {
		t.Fatalf("cleanTranslationOutput() = %q, want %q", got, want)
	}
}

func TestCleanTranslationOutput_PreservesMultilineTranslation(t *testing.T) {
	const source = "Good morning.\n\nHow are you?"
	const generated = "Buenos días.\n\n¿Cómo estás?"

	if got := cleanTranslationOutput(source, generated); got != generated {
		t.Fatalf("cleanTranslationOutput() = %q, want %q", got, generated)
	}
}

func TestBuildPrompt_RequiresExactlyOneTranslation(t *testing.T) {
	prompt := buildPrompt("whenever you are available", "en", "es", "es-CR")
	for _, required := range []string{"exactly one best", "without alternatives", "Preserve the source's line and paragraph structure"} {
		if !strings.Contains(prompt, required) {
			t.Errorf("prompt does not contain %q", required)
		}
	}
}
