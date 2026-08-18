package dacode

import (
	"strings"

	"github.com/semistrict/dago/internal/unicodesecurity"
)

func slashCommandHelp(editor string, glyphs uiGlyphs) string {
	definitions := publicSlashCommandDefinitions()
	lines := []string{"Commands: /help"}
	for _, definition := range definitions {
		if definition.Name == "/help" {
			continue
		}
		label := definition.Name
		if definition.ArgumentHint != "" {
			label += " " + definition.ArgumentHint
		}
		if len(definition.Aliases) > 0 {
			label += " (" + strings.Join(definition.Aliases, ", ") + ")"
		}
		lines = append(lines, label+"  "+glyphs.Bullet+"  "+definition.Description)
	}
	editor = strings.TrimSpace(unicodesecurity.RenderTerminalSafe(editor))
	if editor == "" {
		editor = "external editor"
	}
	lines = append(lines, "", "Ctrl+O expand latest transcript unit  "+glyphs.Bullet+"  Ctrl+X open draft in "+editor)
	return strings.Join(lines, "\n")
}
