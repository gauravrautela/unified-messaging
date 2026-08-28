package model

import (
	"html"
	"regexp"
	"strings"
)

var (
	scriptStyleRE = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)\s*>`)
	tagRE         = regexp.MustCompile(`(?s)<[^>]*>`)
)

// PlainText renders a body as text: markup and script/style blocks stripped
// when bodyType is html, entities decoded, whitespace collapsed. Text bodies
// pass through with whitespace normalised.
func PlainText(body, bodyType string) string {
	text := body
	if bodyType == "html" {
		stripped := scriptStyleRE.ReplaceAllString(body, " ")
		text = html.UnescapeString(tagRE.ReplaceAllString(stripped, " "))
	}
	return strings.Join(strings.Fields(text), " ")
}
