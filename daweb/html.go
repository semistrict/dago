package daweb

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

func looksLikeHTML(value string) bool {
	prefix := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(prefix, "<!doctype html") || strings.HasPrefix(prefix, "<html") ||
		strings.HasPrefix(prefix, "<head") || strings.HasPrefix(prefix, "<body")
}

func htmlToMarkdown(source string) string {
	return htmlToMarkdownWith(source, renderMarkdown)
}

func htmlToMarkdownWith(source string, converter func(string) (string, error)) string {
	converted, err := converter(source)
	if err != nil {
		return fallbackHTMLText(source)
	}
	return converted
}

func renderMarkdown(source string) (string, error) {
	document, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return "", err
	}
	var output strings.Builder
	renderHTML(&output, document, 0, false)
	return normalizeMarkdown(output.String()), nil
}

func renderHTML(output *strings.Builder, node *html.Node, listDepth int, skip bool) {
	if node.Type == html.ElementNode {
		switch node.Data {
		case "script", "style", "noscript", "template", "svg", "canvas":
			skip = true
		case "h1", "h2", "h3", "h4", "h5", "h6":
			output.WriteString("\n\n")
			output.WriteString(strings.Repeat("#", int(node.Data[1]-'0')))
			output.WriteByte(' ')
		case "p", "div", "section", "article", "header", "footer", "main", "blockquote":
			output.WriteString("\n\n")
		case "br":
			output.WriteByte('\n')
		case "ul", "ol":
			listDepth++
			output.WriteByte('\n')
		case "li":
			output.WriteByte('\n')
			output.WriteString(strings.Repeat("  ", max(0, listDepth-1)))
			output.WriteString("- ")
		case "strong", "b":
			output.WriteString("**")
		case "em", "i":
			output.WriteByte('*')
		case "code":
			output.WriteByte('`')
		}
	}
	if node.Type == html.TextNode && !skip {
		text := strings.Join(strings.Fields(node.Data), " ")
		if text != "" {
			if output.Len() > 0 {
				current := output.String()
				last := current[len(current)-1]
				if last != '\n' && last != ' ' && last != '*' && last != '`' {
					output.WriteByte(' ')
				}
			}
			output.WriteString(text)
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		renderHTML(output, child, listDepth, skip)
	}
	if node.Type == html.ElementNode && !skip {
		switch node.Data {
		case "strong", "b":
			output.WriteString("**")
		case "em", "i":
			output.WriteByte('*')
		case "code":
			output.WriteByte('`')
		case "p", "div", "section", "article", "header", "footer", "main", "blockquote",
			"h1", "h2", "h3", "h4", "h5", "h6":
			output.WriteString("\n\n")
		}
	}
}

func fallbackHTMLText(source string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(source))
	var output strings.Builder
	skipDepth := 0
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return normalizeMarkdown(output.String())
		case html.StartTagToken:
			name, _ := tokenizer.TagName()
			switch string(name) {
			case "script", "style", "noscript", "template", "svg", "canvas":
				skipDepth++
			}
		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			switch string(name) {
			case "script", "style", "noscript", "template", "svg", "canvas":
				if skipDepth > 0 {
					skipDepth--
				}
			}
		case html.TextToken:
			if skipDepth == 0 {
				text := strings.Join(strings.Fields(string(tokenizer.Text())), " ")
				if text != "" {
					output.WriteString(text)
					output.WriteString("\n\n")
				}
			}
		}
	}
}

func normalizeMarkdown(value string) string {
	lines := strings.Split(value, "\n")
	result := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if len(result) > 0 && !blank {
				result = append(result, "")
			}
			blank = true
			continue
		}
		result = append(result, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}

func truncateUTF8(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	const marker = "\n\n[content truncated]"
	cut := max(0, limit-len(marker))
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	if cut == 0 {
		markerCut := min(limit, len(marker))
		return marker[:markerCut], true
	}
	return value[:cut] + marker, true
}
