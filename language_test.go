package main

import (
	"strings"
	"testing"
)

func TestLanguageFromCode(t *testing.T) {
	tests := []struct {
		code string
		want string
	}{
		{"de", langGerman},
		{"DE", langGerman},
		{"de-DE", langGerman},
		{"de_CH", langGerman},
		{"deu", langGerman},
		{"ger", langGerman},
		{"en", langEnglish},
		{"en-US", langEnglish},
		{"eng", langEnglish},
		{"fr", ""},
		{"", ""},
		{"  ", ""},
	}
	for _, tc := range tests {
		if got := languageFromCode(tc.code); got != tc.want {
			t.Errorf("languageFromCode(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

const germanSample = `Der Leuchtturm stand auf einer schmalen Landzunge, drei Kilometer vor
dem Dorf, und seit hundertachtzig Jahren brannte sein Licht jede Nacht. Emma hatte
ihn geerbt, zusammen mit dem Logbuch ihres Großvaters und einer Liste von Regeln,
die niemand außer ihr noch für wichtig hielt. Du bleibst hier oben, bis das Licht
brennt, hatte ihr Großvater gesagt. Das Meer fragt nicht, ob du müde bist.`

const englishSample = `The lighthouse stood on a narrow spit of land, three kilometres from the village,
and for a hundred and eighty years its light had burned every night. Emma had
inherited it, together with her grandfather's logbook and a list of rules that
nobody but her still considered important. Stay up here until the light burns,
her grandfather had said. The sea does not ask whether you are tired.`

func TestLanguageFromText(t *testing.T) {
	if got := languageFromText(germanSample); got != langGerman {
		t.Errorf("German sample detected as %q", got)
	}
	if got := languageFromText(englishSample); got != langEnglish {
		t.Errorf("English sample detected as %q", got)
	}
	// A German book that quotes one English line stays German.
	mixed := germanSample + " Someone sang Rule Britannia on the pier. " +
		"Und der alte Mann lachte, denn er wusste, was die Nacht noch bringen würde, " +
		"und er freute sich darauf, obwohl er müde war und seine Hände zitterten."
	if got := languageFromText(mixed); got != langGerman {
		t.Errorf("mixed sample detected as %q, want German", got)
	}
	if got := languageFromText("Zwei Sätze nur."); got != "" {
		t.Errorf("too-short sample decided as %q, want no decision", got)
	}
}

func TestBookLanguagePriority(t *testing.T) {
	if lang, why := bookLanguage("German", "en", englishSample); lang != langGerman || !strings.Contains(why, "override") {
		t.Errorf("override ignored: %q (%s)", lang, why)
	}
	if lang, why := bookLanguage("auto", "de", englishSample); lang != langGerman || !strings.Contains(why, "dc:language") {
		t.Errorf("metadata ignored: %q (%s)", lang, why)
	}
	if lang, why := bookLanguage("auto", "", germanSample); lang != langGerman || !strings.Contains(why, "heuristic") {
		t.Errorf("heuristic ignored: %q (%s)", lang, why)
	}
	if lang, _ := bookLanguage("auto", "", "Knirschen. Knarren."); lang != langEnglish {
		t.Errorf("undecidable input should fall back to English, got %q", lang)
	}
}

func TestLanguageSampleBounded(t *testing.T) {
	chapters := []epubChapter{{Text: strings.Repeat("wort ", 5000)}, {Text: "und tschüss"}}
	sample := languageSample(chapters, 1000)
	if len(sample) > 1100 {
		t.Errorf("sample grew to %d characters, budget was 1000", len(sample))
	}
}
