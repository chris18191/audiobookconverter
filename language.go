package main

import (
	"regexp"
	"strings"
)

// The language names the qwen engine understands as -l argument. Other names
// exist (the model covers ten languages); this converter currently distinguishes
// the two its library and test corpus are made of.
const (
	langGerman  = "German"
	langEnglish = "English"
)

// bookLanguage decides what language the synthesiser must speak, in order of
// confidence: an explicit CONVERTER_TTS_LANGUAGE, the book's dc:language
// metadata, and a function-word heuristic over the extracted text. It returns
// the language and the reason it was chosen, for the log.
func bookLanguage(override, metaLanguage, sample string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "german", "de", "deutsch":
		return langGerman, "CONVERTER_TTS_LANGUAGE override"
	case "english", "en":
		return langEnglish, "CONVERTER_TTS_LANGUAGE override"
	}
	if lang := languageFromCode(metaLanguage); lang != "" {
		return lang, "dc:language " + metaLanguage
	}
	if lang := languageFromText(sample); lang != "" {
		return lang, "text heuristic"
	}
	return langEnglish, "no usable metadata and no decisive text sample"
}

// languageFromCode maps a dc:language value (BCP 47 or ISO 639 codes as books
// write them: "en", "en-US", "de-DE", "deu", "ger") onto a TTS language.
// Unknown languages yield "", so the text heuristic gets a chance.
func languageFromCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if i := strings.IndexAny(code, "-_"); i >= 0 {
		code = code[:i]
	}
	switch code {
	case "de", "deu", "ger":
		return langGerman
	case "en", "eng":
		return langEnglish
	}
	return ""
}

// Function words: they carry no plot, appear with high frequency in any prose
// and almost never overlap between the two languages. The lists are the top
// German/English function words plus the umlaut-bearing ones; words are matched
// case-insensitively after lowercasing (which folds ä ö ü ß correctly).
var (
	englishWordSet = wordSet(`the and of to in that it was for on with as at by
		this from we you your they their he she him his her will would could
		have has had not but are were been said what when who all there`)
	germanWordSet = wordSet(`der die das den dem des und ist nicht von zu mit auf
		für eine ein einer eines ich du er sie wir ihr ihm ihren war werden wird
		auch als bei nach um wie so kann muss schon noch nur beim zum oder aber
		wenn über unter zwischen während wegen`)
	wordRE = regexp.MustCompile(`[a-zäöüß]+`)
)

func wordSet(words string) map[string]bool {
	set := make(map[string]bool)
	for _, w := range strings.Fields(strings.ToLower(words)) {
		set[w] = true
	}
	return set
}

// languageFromText scores a plain-text sample on German and English function
// words. ß is worth extra points: it belongs to German and to no other language
// in this converter's range. A language must score at least three hits and beat
// the other by 15% to win, so a German novel that quotes an English song still
// reads as German, and a text too short to decide falls through to "".
func languageFromText(sample string) string {
	sample = strings.ToLower(sample)
	words := wordRE.FindAllString(sample, -1)
	if len(words) < 30 {
		return ""
	}
	de, en := 0, 0
	for _, w := range words {
		if germanWordSet[w] {
			de++
		} else if englishWordSet[w] {
			en++
		}
	}
	de += 3 * strings.Count(sample, "ß")
	switch {
	case de >= 3 && float64(de) > 1.15*float64(en):
		return langGerman
	case en >= 3 && float64(en) > 1.15*float64(de):
		return langEnglish
	case de >= 3:
		return langGerman // mostly a German text with English quotes
	default:
		return ""
	}
}

// languageSample concatenates the beginnings of the chapters: a few thousand
// characters of real prose settle the language, the whole book is not needed.
func languageSample(chapters []epubChapter, maxChars int) string {
	var b strings.Builder
	for _, ch := range chapters {
		b.WriteString(ch.Text)
		b.WriteByte('\n')
		if b.Len() >= maxChars {
			break
		}
	}
	if b.Len() > maxChars {
		runes := []rune(b.String())
		return string(runes[:min(len(runes), maxChars)])
	}
	return b.String()
}
