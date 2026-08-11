package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	dskill "github.com/semistrict/dago/daskill"
)

//go:embed builtin/*/SKILL.md
var builtinFS embed.FS

// BuiltinSkills returns all skills embedded in the binary.
// These skills have Body set (inline content) and Path empty.
func BuiltinSkills() []Skill {
	var out []Skill

	fs.WalkDir(builtinFS, "builtin", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.ToLower(d.Name()) != "skill.md" {
			return nil
		}

		data, err := builtinFS.ReadFile(path)
		if err != nil {
			panic(fmt.Sprintf("reading embedded skill %s: %v", path, err))
		}

		parsed, _, err := dskill.ParseContent(string(data), path)
		if err != nil {
			panic(fmt.Sprintf("parsing embedded skill %s: %v", path, err))
		}

		// Validate name matches parent directory
		parentDir := filepath.Base(filepath.Dir(path))
		if parsed.Name != parentDir {
			panic(fmt.Sprintf("embedded skill %s: name %q does not match directory %q", path, parsed.Name, parentDir))
		}
		parsed.Path = ""
		out = append(out, parsed)
		return nil
	})

	return out
}
