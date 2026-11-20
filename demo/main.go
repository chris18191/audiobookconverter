// Command demo builds a small German audiobook demo epub: three short original
// chapters with narration and dialogue, a proper dc:language and enough text to
// exercise segmentation. Run it into the watched input folder:
//
//	go run ./demo /path/to/books/demo-deutsch.epub
package main

import (
	"archive/zip"
	"fmt"
	"os"
)

type chapter struct {
	Title string
	Paras []string
}

var chapters = []chapter{
	{"Kapitel Eins", []string{
		"Der Leuchtturm stand auf einer schmalen Landzunge, drei Kilometer vor dem Dorf, und seit hundertachtzig Jahren brannte sein Licht jede Nacht. Emma hatte ihn geerbt, zusammen mit dem Logbuch ihres Großvaters und einer handgeschriebenen Liste von Regeln, die niemand außer ihr noch für wichtig hielt. Die erste Regel lautete: Das Licht brennt, bevor der erste Stern sichtbar wird. Ohne Ausnahme.",
		"Am ersten Morgen kletterte sie die hundertsechsundzwanzig Stufen hinauf und prüfte die Linse. Sie war staubig, aber ganz. Im Wartungsbuch, dick wie eine Enzyklopädie, stand auf der letzten Seite in der zitternden Schrift des Wärters: 14. Oktober, Öl knapp, Bestellung gemacht. Darunter, mit festerer Hand, ein Nachtrag: Emma wird wissen, was zu tun ist.",
		"„Du bleibst hier oben, bis das Licht brennt“, hatte ihr Großvater gesagt, in der Nacht, bevor er einschlief und nicht mehr aufwachte. „Das Meer fragt nicht, ob du müde bist.“ Sie hatte genickt, obwohl er es nicht mehr sehen konnte, und seitdem war sie hier.",
	}},
	{"Kapitel Zwei", []string{
		"Der Sturm kam drei Wochen später, angekündigt von einem Barometerstand, den Emma für einen Druckfehler hielt. Er fiel und fiel, und gegen acht Uhr abends riss der Wind das erste Fenster im Wohnhaus ein. Das Meer warf seine Gischt bis zur halben Tür des Turms, und über dem Heulen des Windes hörte sie nichts als das gleichmäßige Schlagen des Nebelhorns, das sie am Nachmittag eingeschaltet hatte.",
		"Um Mitternacht blieb die Maschine stehen. Der Notgenerator sprang nicht an, dann doch, dann wieder nicht, und die Dunkelheit draußen wurde mit jedem Augenblick voller. Emma zog ihre Gummistiefel an, kroch in den Maschinenraum und legte die Hand auf den heißen Motorblock. Warm. Nur der Anlasser. Sie erinnerte sich an Regel sieben: Vor dem dritten Versuch den Riemen prüfen, nie danach.",
		"„Wenn das Licht ausgeht“, hatte der Lotsenverband in einem Brief gedroht, den sie im Flur fand, „dann geht das Dorf mit aus.“ Fünfhundert Menschen, ein Hafen, ein Wrack pro Sturm ohne Licht. Sie legte den Riemen neu, trocknete die Zündkerzen an ihrem Ärmel und drehte den Anlasser ein letztes Mal. Die Maschine hustete, fluchte und lief.",
	}},
	{"Kapitel Drei", []string{
		"Am Morgen lag das Land unter einer Decke aus Tang und Vogelfedern, aber das Licht hatte gebrannt. Emma schrieb die erste eigene Zeile ins Logbuch: 3. November, Wind aus West, Stärke neun. Das Licht brannte die ganze Nacht. Darunter setzte sie, nach kurzem Zögern, ihren Namen, vollständig, wie ihr Großvater es immer gemacht hatte.",
		"Die Fischer kamen gegen Mittag zurück, drei Kutter, nass und still. Der älteste von ihnen, ein Mann namens Tiedemann, blieb am Fuß des Turms stehen und hob nur die Hand. Mehr brauchte es nicht. Oben, in der Laterne, drehte sich die Linse in ihrem Ölbad und warf das Restlicht des Tages in hundert kleine Sonnen über das Wasser.",
		"Emma kochte Kaffee, setzte sich auf die breite Fensterbank und schlug das Logbuch wieder auf. Zwischen den letzten Seiten fand sie ein Foto: ihr Großvater als junger Mann, die Hand auf derselben Maschine, das Gesicht rußgeschwärzt und lachend. Auf der Rückseite stand, in derselben festen Hand wie der Nachtrag: „Regel null: Man bleibt, weil es jemanden braucht.“ Sie sah hinaus aufs Meer, das jetzt friedlich war, und nickte.",
	}},
}

const (
	title    = "Der Leuchtturm"
	creator  = "Ein Demo-Autor"
	language = "de"
)

func document(c chapter) string {
	body := ""
	for _, p := range c.Paras {
		body += "<p>" + p + "</p>\n"
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<html xmlns="http://www.w3.org/1999/xhtml" xml:lang="de"><head><title>%s</title></head>
<body><h1>%s</h1>
%s</body></html>`, c.Title, c.Title, body)
}

func main() {
	dest := "demo-deutsch.epub"
	if len(os.Args) > 1 {
		dest = os.Args[1]
	}
	f, err := os.Create(dest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	zw := zip.NewWriter(f)

	add := func(name, content string) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(content))
		return err
	}

	mimetype, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		panic(err)
	}
	if _, err := mimetype.Write([]byte("application/epub+zip")); err != nil {
		panic(err)
	}
	if err := add("META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`); err != nil {
		panic(err)
	}

	manifest, spine := "", ""
	for i := range chapters {
		manifest += fmt.Sprintf(`    <item id="ch%d" href="chapter%d.xhtml" media-type="application/xhtml+xml"/>`+"\n", i+1, i+1)
		spine += fmt.Sprintf("      <itemref idref=\"ch%d\"/>\n", i+1)
	}
	opf := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="uid" xml:lang="de">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="uid">urn:uuid:0f7f2a86-6c1e-4a5a-9d3c-demo-deutsch</dc:identifier>
    <dc:title>%s</dc:title>
    <dc:creator>%s</dc:creator>
    <dc:language>%s</dc:language>
  </metadata>
  <manifest>
%s  </manifest>
  <spine>
%s  </spine>
</package>`, title, creator, language, manifest, spine)
	if err := add("OEBPS/content.opf", opf); err != nil {
		panic(err)
	}
	for i, c := range chapters {
		if err := add(fmt.Sprintf("OEBPS/chapter%d.xhtml", i+1), document(c)); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %s (%d chapters)\n", dest, len(chapters))
}
