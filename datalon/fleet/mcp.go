package fleet

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const interruptToolsEnv = "DEEPAGENTS_TALON_INTERRUPT_ON_TOOLS"

func collectTools(ctx context.Context, entries map[string]*zip.File, limits Limits) ([]serverSummary, []string, error) {
	type manifest struct {
		name  string
		scope string
	}
	manifests := make([]manifest, 0)
	if entries["tools.json"] != nil {
		manifests = append(manifests, manifest{name: "tools.json", scope: "root"})
	}
	for name := range entries {
		if isSubagentFile(name, "tools.json") {
			parts := strings.Split(name, "/")
			manifests = append(manifests, manifest{name: name, scope: parts[1]})
		}
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].name < manifests[j].name })

	grouped := make(map[string]*serverSummary)
	totalTools := 0
	for _, manifest := range manifests {
		requests, err := parseToolManifest(ctx, entries[manifest.name], manifest.name, manifest.scope, limits)
		if err != nil {
			return nil, nil, err
		}
		if len(requests) > limits.MaxTools-totalTools {
			return nil, nil, fmt.Errorf("%w: more than %d tools", ErrLimitExceeded, limits.MaxTools)
		}
		totalTools += len(requests)
		for _, request := range requests {
			key := request.serverName + "\x00" + request.serverURL
			summary := grouped[key]
			if summary == nil {
				summary = &serverSummary{
					serverURL: request.serverURL, serverName: request.serverName,
					scopes: make(map[string]struct{}), tools: make(map[string]struct{}),
					interrupts: make(map[string]struct{}),
				}
				grouped[key] = summary
			}
			summary.scopes[request.scope] = struct{}{}
			summary.tools[request.name] = struct{}{}
			if request.interrupt {
				summary.interrupts[request.name] = struct{}{}
			}
		}
	}

	summaries := make([]serverSummary, 0, len(grouped))
	interruptSet := make(map[string]struct{})
	for _, summary := range grouped {
		summaries = append(summaries, *summary)
		for name := range summary.interrupts {
			interruptSet[name] = struct{}{}
		}
	}
	sort.Slice(summaries, func(i, j int) bool {
		left, right := strings.ToLower(summaries[i].serverName), strings.ToLower(summaries[j].serverName)
		if left == right {
			return summaries[i].serverURL < summaries[j].serverURL
		}
		return left < right
	})
	return summaries, sortedSet(interruptSet), nil
}

func parseToolManifest(ctx context.Context, entry *zip.File, name, scope string, limits Limits) ([]toolRequest, error) {
	data, err := readEntry(ctx, entry, limits.MaxFileBytes)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("%w: %s contains malformed JSON", ErrInvalidTools, name)
	}
	if document == nil {
		return nil, fmt.Errorf("%w: %s must contain an object", ErrInvalidTools, name)
	}
	rawTools, ok := document["tools"].([]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s must contain a tools list", ErrInvalidTools, name)
	}
	if len(rawTools) > limits.MaxTools {
		return nil, fmt.Errorf("%w: %s contains more than %d tools", ErrLimitExceeded, name, limits.MaxTools)
	}
	interrupts, _ := document["interrupt_config"].(map[string]any)
	requests := make([]toolRequest, 0, len(rawTools))
	for index, raw := range rawTools {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: %s tools[%d] must be an object", ErrInvalidTools, name, index)
		}
		toolName, err := requiredManifestString(item, "name", name, index)
		if err != nil {
			return nil, err
		}
		if !toolNamePattern.MatchString(toolName) {
			return nil, fmt.Errorf("%w: %s tools[%d].name contains unsupported characters", ErrInvalidTools, name, index)
		}
		rawURL, err := requiredManifestString(item, "mcp_server_url", name, index)
		if err != nil {
			return nil, err
		}
		serverURL, err := sanitizeServerURL(rawURL)
		if err != nil {
			return nil, fmt.Errorf("%w: %s tools[%d].mcp_server_url is not a valid HTTP URL", ErrInvalidTools, name, index)
		}
		serverName, err := requiredManifestString(item, "mcp_server_name", name, index)
		if err != nil {
			return nil, err
		}
		if len(serverName) > 256 || strings.IndexFunc(serverName, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("%w: %s tools[%d].mcp_server_name is unsafe", ErrInvalidTools, name, index)
		}
		interrupt := item["interrupt_config"] == true
		if !interrupt {
			keys := []string{
				serverURL + "::" + toolName + "::" + serverName,
				rawURL + "::" + toolName + "::" + serverName,
				toolName,
			}
			for _, key := range keys {
				if interrupts[key] == true {
					interrupt = true
					break
				}
			}
		}
		requests = append(requests, toolRequest{
			name: toolName, serverURL: serverURL, serverName: serverName,
			scope: scope, interrupt: interrupt,
		})
	}
	return requests, nil
}

func requiredManifestString(item map[string]any, key, manifest string, index int) (string, error) {
	value, ok := item[key].(string)
	value = strings.TrimSpace(value)
	if !ok || value == "" {
		return "", fmt.Errorf("%w: %s tools[%d].%s must be a non-empty string", ErrInvalidTools, manifest, index, key)
	}
	return value, nil
}

func readEntry(ctx context.Context, entry *zip.File, maximum uint64) ([]byte, error) {
	if entry == nil {
		return nil, fmt.Errorf("%w: missing archive entry", ErrInvalidArchive)
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("read Fleet entry %s: %w", entry.Name, err)
	}
	defer reader.Close()
	result := make([]byte, 0, min(entry.UncompressedSize64, uint64(64<<10)))
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, readErr := reader.Read(buffer)
		if count > 0 {
			if uint64(count) > maximum-uint64(len(result)) || uint64(len(result)+count) > entry.UncompressedSize64 {
				return nil, fmt.Errorf("%w: %s expanded beyond its declared size", ErrLimitExceeded, entry.Name)
			}
			result = append(result, buffer[:count]...)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read Fleet entry %s: %w", entry.Name, readErr)
		}
	}
	if uint64(len(result)) != entry.UncompressedSize64 {
		return nil, fmt.Errorf("%w: %s size does not match its header", ErrUnsafeArchive, entry.Name)
	}
	return result, nil
}

func sanitizeServerURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return "", errors.New("invalid HTTP URL")
	}
	if parsed.Opaque != "" || strings.ContainsAny(parsed.Hostname(), "\r\n\x00") {
		return "", errors.New("invalid HTTP URL")
	}
	port := parsed.Port()
	if port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return "", errors.New("invalid HTTP URL")
		}
	}
	host := parsed.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	clean := &url.URL{Scheme: parsed.Scheme, Host: host, Path: sanitizeURLPath(parsed.Path)}
	rendered := clean.String()
	return strings.ReplaceAll(rendered, "%3Csecret-redacted%3E", "<secret-redacted>"), nil
}

func sanitizeURLPath(raw string) string {
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == '/' })
	if len(parts) == 0 {
		return ""
	}
	clean := make([]string, 0, len(parts))
	redactNext := false
	for _, part := range parts {
		marker := secretMarker.MatchString(part)
		if redactNext || marker || secretPart.MatchString(part) {
			clean = append(clean, "<secret-redacted>")
		} else {
			clean = append(clean, part)
		}
		redactNext = marker
	}
	return "/" + strings.Join(clean, "/")
}

func formatMCPConfig(summaries []serverSummary) (string, error) {
	servers := make(map[string]any, len(summaries))
	for _, summary := range summaries {
		id := uniqueServerID(serverID(summary), servers)
		servers[id] = serverConfig(summary)
	}
	data, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode sanitized MCP config: %w", err)
	}
	return string(data) + "\n", nil
}

func formatSetup(source string, summaries []serverSummary) string {
	lines := []string{
		fmt.Sprintf("Fleet MCP setup notes for %q", source),
		"",
		"Generated .mcp.json contains the sanitized suggested server configuration.",
	}
	for _, summary := range summaries {
		tools := sortedSet(summary.tools)
		fragment, _ := json.MarshalIndent(map[string]any{serverID(summary): serverConfig(summary)}, "", "  ")
		lines = append(lines,
			"",
			"Server: "+summary.serverName,
			"URL: "+summary.serverURL,
			fmt.Sprintf("Tool count: %d", len(tools)),
			"Scopes: "+strings.Join(sortedSet(summary.scopes), ", "),
			"Requested tools:",
		)
		for _, tool := range tools {
			lines = append(lines, "- "+tool)
		}
		interrupts := sortedSet(summary.interrupts)
		label := "none"
		if len(interrupts) > 0 {
			label = strings.Join(interrupts, ", ")
		}
		lines = append(lines,
			"Interrupt-enabled tools: "+label,
			"",
			"Suggested .mcp.json fragment:",
			string(fragment),
		)
	}
	return strings.Join(lines, "\n") + "\n"
}

func serverConfig(summary serverSummary) map[string]any {
	return map[string]any{
		"type": "http", "url": summary.serverURL, "auth": "oauth",
		"allowedTools": sortedSet(summary.tools),
	}
}

func serverID(summary serverSummary) string {
	raw := strings.TrimSpace(strings.ToLower(summary.serverName))
	if raw == "" {
		if parsed, err := url.Parse(summary.serverURL); err == nil {
			raw = parsed.Hostname()
		}
	}
	id := strings.Trim(serverIDPart.ReplaceAllString(raw, "-"), "-")
	if id == "" {
		return "server"
	}
	return id
}

func uniqueServerID(id string, servers map[string]any) string {
	if _, exists := servers[id]; !exists {
		return id
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", id, suffix)
		if _, exists := servers[candidate]; !exists {
			return candidate
		}
	}
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
