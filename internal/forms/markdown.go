package forms

import (
	"html"
	"html/template"
	"regexp"
	"strings"
)

var (
	mdLink   = regexp.MustCompile(`\[([^\]\n]{1,200})\]\((https://[^\s()<>"]{1,500})\)`)
	mdBold   = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdItalic = regexp.MustCompile(`(^|[^*])\*([^*\n]+)\*`)
)

// markdown convertit le Markdown réduit des formulaires (gras, italique,
// liens https si links, listes « - ») en HTML : tout le reste est échappé
// et reste du texte.
func markdown(src string, links bool) template.HTML {
	var b strings.Builder
	inList := false
	para := []string{}
	flush := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + strings.Join(para, "<br>") + "</p>")
			para = para[:0]
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		item, isItem := strings.CutPrefix(t, "- ")
		switch {
		case isItem:
			flush()
			if !inList {
				b.WriteString("<ul>")
				inList = true
			}
			b.WriteString("<li>" + inline(item, links) + "</li>")
		case t == "":
			flush()
			if inList {
				b.WriteString("</ul>")
				inList = false
			}
		default:
			if inList {
				b.WriteString("</ul>")
				inList = false
			}
			para = append(para, inline(t, links))
		}
	}
	flush()
	if inList {
		b.WriteString("</ul>")
	}
	return template.HTML(b.String())
}

func inline(s string, links bool) string {
	s = html.EscapeString(s)
	if links {
		// Les adresses ont été échappées ; &amp; reste valide dans href.
		s = mdLink.ReplaceAllString(s, `<a href="$2" rel="noopener noreferrer" target="_blank">$1</a>`)
	}
	s = mdBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = mdItalic.ReplaceAllString(s, "$1<em>$2</em>")
	return s
}
