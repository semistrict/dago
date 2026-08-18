package dacode

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"strings"
)

const (
	itermCursorGuideOff = "\x1b]1337;HighlightCursorLine=no\x1b\\"
	itermCursorGuideOn  = "\x1b]1337;HighlightCursorLine=yes\x1b\\"
	itermPreferencesMax = 4 << 20
)

type itermCursorGuide struct {
	Disable string
	Restore string
}

func resolveITermCursorGuide(lookup func(string) (string, bool), stderrTTY bool, preferences io.Reader) itermCursorGuide {
	if !stderrTTY || lookup == nil || !isITermEnvironment(lookup) || preferences == nil {
		return itermCursorGuide{}
	}
	active, _ := lookup("ITERM_PROFILE")
	enabled, err := itermProfileCursorGuideEnabled(io.LimitReader(preferences, itermPreferencesMax+1), strings.TrimSpace(active))
	if err != nil || !enabled {
		return itermCursorGuide{}
	}
	return itermCursorGuide{Disable: itermCursorGuideOff, Restore: itermCursorGuideOn}
}

func loadITermCursorGuide(lookup func(string) (string, bool), stderrTTY bool, path string) itermCursorGuide {
	if !stderrTTY || lookup == nil || !isITermEnvironment(lookup) || strings.TrimSpace(path) == "" {
		return itermCursorGuide{}
	}
	file, err := os.Open(path)
	if err != nil {
		return itermCursorGuide{}
	}
	defer file.Close()
	return resolveITermCursorGuide(lookup, stderrTTY, file)
}

func suspendITermCursorGuide(output io.Writer, guide itermCursorGuide) func() {
	if output == nil || guide.Disable == "" || guide.Restore == "" {
		return func() {}
	}
	written, _ := io.WriteString(output, guide.Disable)
	if written == 0 {
		return func() {}
	}
	return func() { _, _ = io.WriteString(output, guide.Restore) }
}

func isITermEnvironment(lookup func(string) (string, bool)) bool {
	if value, exists := lookup("LC_TERMINAL"); exists && value == "iTerm2" {
		return true
	}
	value, _ := lookup("TERM_PROGRAM")
	return value == "iTerm.app"
}

func itermProfileCursorGuideEnabled(reader io.Reader, activeProfile string) (bool, error) {
	decoder := xml.NewDecoder(reader)
	var (
		dictionaries []map[string]string
		stack        []map[string]string
		pendingKeys  []string
		bytesRead    int64
	)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, err
		}
		if decoder.InputOffset() > itermPreferencesMax {
			return false, errors.New("iTerm preferences exceed the supported size")
		}
		bytesRead = decoder.InputOffset()
		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "dict":
				stack = append(stack, map[string]string{})
				pendingKeys = append(pendingKeys, "")
			case "key", "string", "integer":
				if len(stack) == 0 {
					continue
				}
				var value string
				if err := decoder.DecodeElement(&value, &element); err != nil {
					return false, err
				}
				value = strings.TrimSpace(value)
				last := len(stack) - 1
				if element.Name.Local == "key" {
					pendingKeys[last] = value
				} else if pendingKeys[last] != "" {
					stack[last][pendingKeys[last]] = value
					pendingKeys[last] = ""
				}
			case "true", "false":
				if len(stack) == 0 {
					continue
				}
				last := len(stack) - 1
				if pendingKeys[last] != "" {
					stack[last][pendingKeys[last]] = element.Name.Local
					pendingKeys[last] = ""
				}
			}
		case xml.EndElement:
			if element.Name.Local == "dict" && len(stack) > 0 {
				last := len(stack) - 1
				dictionaries = append(dictionaries, stack[last])
				stack = stack[:last]
				pendingKeys = pendingKeys[:last]
			}
		}
	}
	if bytesRead > itermPreferencesMax {
		return false, errors.New("iTerm preferences exceed the supported size")
	}
	defaultGUID := ""
	for _, dictionary := range dictionaries {
		if value := dictionary["Default Bookmark Guid"]; value != "" {
			defaultGUID = value
		}
	}
	for _, preferName := range []bool{true, false} {
		for _, profile := range dictionaries {
			matches := preferName && activeProfile != "" && profile["Name"] == activeProfile
			if !preferName {
				matches = defaultGUID != "" && profile["Guid"] == defaultGUID
			}
			if matches {
				return profileUsesCursorGuide(profile), nil
			}
		}
	}
	return false, nil
}

func profileUsesCursorGuide(profile map[string]string) bool {
	if value, exists := profile["Use Cursor Guide"]; exists {
		return value == "true" || (value != "false" && value != "0")
	}
	for _, key := range []string{"Use Cursor Guide (Dark)", "Use Cursor Guide (Light)"} {
		if value := profile[key]; value == "true" || (value != "" && value != "false" && value != "0") {
			return true
		}
	}
	return false
}
