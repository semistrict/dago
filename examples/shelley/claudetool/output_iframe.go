package claudetool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

// OutputIframeTool displays sandboxed HTML content to the user.
// It requires a MutableWorkingDir to resolve relative file paths.
type OutputIframeTool struct {
	WorkingDir *MutableWorkingDir
}

func (t *OutputIframeTool) NativeTool() datool.Tool {
	return datool.MustNew(outputIframeName, outputIframeDescription, func(_ context.Context, input outputIframeInput) (datool.Result, error) {
		display, err := t.execute(input)
		if err != nil {
			return datool.Result{}, err
		}
		artifact, err := json.Marshal(display)
		if err != nil {
			return datool.Result{}, fmt.Errorf("encode output iframe display: %w", err)
		}
		return datool.Result{
			Content:  []dmessage.ContentBlock{{Type: dmessage.BlockText, Text: "displayed"}},
			Artifact: artifact,
		}, nil
	})
}

const (
	outputIframeName        = "output_iframe"
	outputIframeDescription = `Display HTML content to the user in a sandboxed iframe.

Use this tool for visualizations like charts, graphs, and HTML demos that the user should see.
The HTML will be rendered in a secure sandbox with scripts enabled but isolated from the parent page.

Do NOT use this tool for:
- Regular text responses (use normal messages instead)
- File operations (use patch or bash)
- Simple data display (just describe it in text)

Good uses:
- Vega-Lite or other chart library visualizations  
- HTML/CSS demonstrations
- Interactive widgets or mini-apps
- SVG graphics

The HTML should be self-contained. You can include inline <script> and <style> tags.

External resources can be loaded via CDN (e.g., https://cdn.jsdelivr.net/).

For visualizations that need external data files (JSON, CSV, etc.), use the 'files' parameter
to bundle them. They will be injected into the page and accessible via window.__FILES__['filename']
for JSON/text/JS (as raw strings), or as <style> tags for CSS files.

For large skill runtimes (excalidraw, etc.), use the 'libraries' parameter instead of 'files'.
Libraries are streamed into the sandboxed iframe by the host page (so their bytes are not stored
in the conversation) and exposed as a Promise at window.__LIBS__:
  const { render } = (await window.__LIBS__).excalidraw;
Valid library names: "excalidraw".`
)

// allowedLibraries maps library names (as the agent specifies them) to the
// /static/ path the host React component fetches. Adding a new entry is the
// only change needed to expose another runtime to output_iframe skills.
var allowedLibraries = map[string]string{
	"excalidraw": "/static/excalidraw/skill.js",
}

// EmbeddedFile represents a file bundled with the HTML.
type EmbeddedFile struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Type    string `json:"type"` // "json", "css", "js", "text"
}

// OutputIframeDisplay is the data passed to the UI for rendering.
type OutputIframeDisplay struct {
	Type     string         `json:"type"`
	HTML     string         `json:"html"`
	Title    string         `json:"title,omitempty"`
	Filename string         `json:"filename,omitempty"`
	Files    []EmbeddedFile `json:"files,omitempty"`
	// Libraries are names of shelley-hosted /static/ runtimes the iframe
	// should preload. The host page (OutputIframeTool.tsx) fetches each
	// library from same-origin and postMessages it in — so the bytes are
	// not stored in the conversation. Resolved into window.__LIBS__ inside
	// the iframe by an injected bootstrap script.
	Libraries []string `json:"libraries,omitempty"`
}

// detectFileType guesses the file type from the filename.
func detectFileType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".json":
		return "json"
	case ".css":
		return "css"
	case ".js":
		return "js"
	case ".csv":
		return "csv"
	default:
		return "text"
	}
}

// injectFiles modifies the HTML to make bundled files accessible.
// CSS files are injected as <style> tags. Everything else (JSON, CSV, JS,
// text) is surfaced as a raw string under window.__FILES__[name] so the
// page decides how to use it (e.g. JSON.parse, or wrap JS in a Blob and
// import() the resulting blob: URL — the way sandboxed iframes can load
// large modules without tripping cross-origin restrictions).
func injectFiles(html string, files []EmbeddedFile) string {
	if len(files) == 0 {
		return html
	}

	var cssFiles []EmbeddedFile
	var dataFiles []EmbeddedFile

	for _, f := range files {
		switch f.Type {
		case "css":
			cssFiles = append(cssFiles, f)
		default:
			// JS, JSON, CSV, text — all surfaced as raw strings under
			// window.__FILES__. The page chooses how to evaluate them
			// (e.g. JSON.parse, or wrap JS in a Blob and import()).
			dataFiles = append(dataFiles, f)
		}
	}

	var injection strings.Builder

	// Inject CSS files as style tags
	for _, f := range cssFiles {
		injection.WriteString("<style data-file=\"")
		injection.WriteString(f.Name)
		injection.WriteString("\">\n")
		injection.WriteString(f.Content)
		injection.WriteString("\n</style>\n")
	}

	// Inject data files as window.__FILES__
	if len(dataFiles) > 0 {
		injection.WriteString("<script>\nwindow.__FILES__ = window.__FILES__ || {};\n")
		for _, f := range dataFiles {
			// Escape the content for JavaScript string
			escaped := escapeJSString(f.Content)
			injection.WriteString("window.__FILES__[\"")
			injection.WriteString(f.Name)
			injection.WriteString("\"] = \"")
			injection.WriteString(escaped)
			injection.WriteString("\";\n")
		}
		injection.WriteString("</script>\n")
	}

	// Insert after <head> or at the beginning
	injectionStr := injection.String()
	if idx := strings.Index(strings.ToLower(html), "<head>"); idx != -1 {
		return html[:idx+6] + "\n" + injectionStr + html[idx+6:]
	}
	if idx := strings.Index(strings.ToLower(html), "<html>"); idx != -1 {
		return html[:idx+6] + "\n<head>\n" + injectionStr + "</head>\n" + html[idx+6:]
	}
	// No head or html tag, just prepend
	return injectionStr + html
}

// escapeJSString escapes a string for use in a JavaScript string literal
// embedded inside an inline HTML <script> element.
//
// HTML's script-data tokenization is tricky: encountering `<!--` followed by
// `<script` switches the parser into the "script data double escape" state
// where `</script>` is no longer recognized as a close tag until a matching
// `-->` returns to normal. A minified JS bundle that contains the substrings
// `<!--` and `<script` (e.g. for innerHTML or doc-fragment work) will break
// the host page and spill its contents as text after our own `</script>`.
//
// We sidestep both by escaping any `</` to `<\/` and any `<!--` to `<\!--`
// inside the resulting JS string literal — both forms are equivalent at the
// JS-source level but never enter HTML's script-escape modes.
//
// We also escape U+2028/U+2029, which are valid in JSON but unescaped
// terminate JS string literals — a real-world hazard when bundling
// minified third-party JS as a __FILES__ entry.
func escapeJSString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			b.WriteString("\\\\")
		case '"':
			b.WriteString("\\\"")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		case '<':
			rest := s[i+1:]
			switch {
			case strings.HasPrefix(rest, "/"):
				b.WriteString("<\\/")
				i++
			case strings.HasPrefix(rest, "!--"):
				b.WriteString("<\\!--")
				i += 3
			default:
				b.WriteByte('<')
			}
		case 0xE2:
			// U+2028 LINE SEPARATOR = E2 80 A8, U+2029 PARAGRAPH SEPARATOR = E2 80 A9.
			if i+2 < len(s) && s[i+1] == 0x80 && (s[i+2] == 0xA8 || s[i+2] == 0xA9) {
				if s[i+2] == 0xA8 {
					b.WriteString("\\u2028")
				} else {
					b.WriteString("\\u2029")
				}
				i += 2
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

type outputIframeInput struct {
	Path      string            `json:"path" description:"Path to the HTML file to display. Relative paths are resolved from the working directory."`
	Title     string            `json:"title,omitempty" description:"Optional title describing the visualization"`
	Files     map[string]string `json:"files,omitempty" description:"Additional small files to bundle. Keys are names used in the HTML and values are file paths."`
	Libraries []string          `json:"libraries,omitempty" description:"Names of hosted runtime libraries to load. Allowed: excalidraw."`
}

func (t *OutputIframeTool) execute(input outputIframeInput) (OutputIframeDisplay, error) {
	if input.Path == "" {
		return OutputIframeDisplay{}, fmt.Errorf("path is required")
	}

	// Resolve the path relative to working directory
	path := input.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.WorkingDir.Get(), path)
	}

	// Read the main HTML file
	data, err := os.ReadFile(path)
	if err != nil {
		return OutputIframeDisplay{}, fmt.Errorf("failed to read file: %v", err)
	}

	// Read additional files
	var embeddedFiles []EmbeddedFile
	for name, filePath := range input.Files {
		// Resolve relative paths
		if !filepath.IsAbs(filePath) {
			filePath = filepath.Join(t.WorkingDir.Get(), filePath)
		}
		content, err := os.ReadFile(filePath)
		if err != nil {
			return OutputIframeDisplay{}, fmt.Errorf("failed to read file %q: %v", name, err)
		}
		embeddedFiles = append(embeddedFiles, EmbeddedFile{
			Name:    name,
			Path:    input.Files[name], // Original path for download
			Content: string(content),
			Type:    detectFileType(name),
		})
	}

	// Inject files into the HTML for iframe display
	html := injectFiles(string(data), embeddedFiles)

	// Validate library names against the allowlist (also drops dupes).
	var libs []string
	seen := map[string]bool{}
	for _, name := range input.Libraries {
		if _, ok := allowedLibraries[name]; !ok {
			return OutputIframeDisplay{}, fmt.Errorf("unknown library %q (allowed: see tool description)", name)
		}
		if !seen[name] {
			seen[name] = true
			libs = append(libs, name)
		}
	}

	return OutputIframeDisplay{
		Type:      "output_iframe",
		HTML:      html,
		Title:     input.Title,
		Filename:  filepath.Base(input.Path),
		Files:     embeddedFiles,
		Libraries: libs,
	}, nil
}
