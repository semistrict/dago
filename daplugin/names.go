package daplugin

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

var unsafeServerName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// NamespacedSkillName qualifies a recursively discovered skill beneath its plugin.
func NamespacedSkillName(pluginID, name string, folders ...string) string {
	parts := append([]string{pluginID}, folders...)
	parts = append(parts, name)
	return strings.ToLower(strings.Join(parts, ":"))
}

// ScopedMCPName produces a collision-resistant globally valid server name.
func ScopedMCPName(pluginID, server string) string {
	if strings.TrimSpace(pluginID) == "" || strings.TrimSpace(server) == "" {
		panic("daplugin: plugin and MCP server names are required")
	}
	return "plugin__" + safeServerPart(pluginID) + "__" + safeServerPart(server)
}

func safeServerPart(value string) string {
	clean := strings.Trim(unsafeServerName.ReplaceAllString(value, "_"), "_")
	if clean == value && clean != "" && len(clean) <= 48 {
		return clean
	}
	if clean == "" {
		clean = "unnamed"
	}
	if len(clean) > 48 {
		clean = clean[:48]
	}
	digest := sha256.Sum256([]byte(value))
	return clean + "_" + hex.EncodeToString(digest[:4])
}
