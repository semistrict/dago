package backend

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type route struct {
	prefix  string
	backend Backend
}

// Composite routes virtual paths by normalized longest prefix.
type Composite struct {
	defaultBackend Backend
	routes         []route
}

func (composite *Composite) runtimeBackends() []RuntimeBackend {
	seen := map[RuntimeBackend]bool{}
	var result []RuntimeBackend
	add := func(value Backend) {
		if runtime, ok := value.(RuntimeBackend); ok && !seen[runtime] {
			seen[runtime] = true
			result = append(result, runtime)
		}
	}
	add(composite.defaultBackend)
	for _, route := range composite.routes {
		add(route.backend)
	}
	return result
}

func (composite *Composite) BindRuntime(ctx context.Context, reader StateReader) (context.Context, error) {
	var err error
	for _, value := range composite.runtimeBackends() {
		ctx, err = value.BindRuntime(ctx, reader)
		if err != nil {
			return nil, err
		}
	}
	return ctx, nil
}

func (composite *Composite) RuntimeUpdates(ctx context.Context) map[string]any {
	result := map[string]any{}
	for _, value := range composite.runtimeBackends() {
		for key, update := range value.RuntimeUpdates(ctx) {
			if previous, exists := result[key]; exists {
				result[key] = mergeStatePatches(previous, update)
			} else {
				result[key] = update
			}
		}
	}
	return result
}

func (composite *Composite) StateFields() []StateField {
	byKey := map[string]StateField{}
	for _, value := range composite.runtimeBackends() {
		for _, field := range value.StateFields() {
			byKey[field.Key] = field
		}
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]StateField, 0, len(keys))
	for _, key := range keys {
		result = append(result, byKey[key])
	}
	return result
}

func mergeStatePatches(left, right any) any {
	leftMap, leftOK := left.(map[string]any)
	rightMap, rightOK := right.(map[string]any)
	if !leftOK || !rightOK {
		return right
	}
	result := cloneStatePatch(leftMap)
	for key, value := range rightMap {
		result[key] = value
	}
	return result
}

func NewComposite(defaultBackend Backend, routes map[string]Backend) (*Composite, error) {
	if defaultBackend == nil {
		return nil, fmt.Errorf("composite default backend is required")
	}
	result := &Composite{defaultBackend: defaultBackend}
	for prefix, value := range routes {
		if value == nil || !strings.HasPrefix(prefix, "/") {
			return nil, fmt.Errorf("composite route %q is invalid", prefix)
		}
		normalized, err := normalizeVirtual(prefix)
		if err != nil || normalized == "/" {
			return nil, fmt.Errorf("composite route %q is invalid", prefix)
		}
		result.routes = append(result.routes, route{prefix: strings.TrimSuffix(normalized, "/") + "/", backend: value})
	}
	sort.Slice(result.routes, func(i, j int) bool {
		if len(result.routes[i].prefix) != len(result.routes[j].prefix) {
			return len(result.routes[i].prefix) > len(result.routes[j].prefix)
		}
		return result.routes[i].prefix < result.routes[j].prefix
	})
	return result, nil
}

func (composite *Composite) selectBackend(value string) (Backend, string, string) {
	for _, route := range composite.routes {
		root := strings.TrimSuffix(route.prefix, "/")
		if value == root {
			return route.backend, "/", route.prefix
		}
		if strings.HasPrefix(value, route.prefix) {
			return route.backend, "/" + strings.TrimPrefix(value, route.prefix), route.prefix
		}
	}
	return composite.defaultBackend, value, ""
}

func remapPath(value, prefix string) string {
	if prefix == "" {
		return value
	}
	root := strings.TrimSuffix(prefix, "/")
	if value == "/" {
		return root
	}
	return root + "/" + strings.TrimPrefix(value, "/")
}

func (composite *Composite) List(ctx context.Context, value string) (ListResult, error) {
	backend, inner, prefix := composite.selectBackend(value)
	result, err := backend.List(ctx, inner)
	if err != nil {
		return ListResult{}, err
	}
	for index := range result.Entries {
		trailing := strings.HasSuffix(result.Entries[index].Path, "/")
		result.Entries[index].Path = remapPath(strings.TrimSuffix(result.Entries[index].Path, "/"), prefix)
		if trailing {
			result.Entries[index].Path += "/"
		}
	}
	if value == "/" {
		seen := map[string]bool{}
		for _, entry := range result.Entries {
			seen[entry.Path] = true
		}
		for _, route := range composite.routes {
			first := strings.Split(strings.Trim(route.prefix, "/"), "/")[0]
			path := "/" + first + "/"
			if !seen[path] {
				result.Entries = append(result.Entries, FileInfo{Path: path, IsDir: true})
				seen[path] = true
			}
		}
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path < result.Entries[j].Path })
	return result, nil
}

func (composite *Composite) Read(ctx context.Context, value string, offset, limit int) (ReadResult, error) {
	backend, inner, _ := composite.selectBackend(value)
	return backend.Read(ctx, inner, offset, limit)
}
func (composite *Composite) Write(ctx context.Context, value, content string) (WriteResult, error) {
	backend, inner, prefix := composite.selectBackend(value)
	result, err := backend.Write(ctx, inner, content)
	result.Path = remapPath(result.Path, prefix)
	return result, err
}
func (composite *Composite) Edit(ctx context.Context, value, old, replacement string, all bool) (EditResult, error) {
	backend, inner, prefix := composite.selectBackend(value)
	result, err := backend.Edit(ctx, inner, old, replacement, all)
	result.Path = remapPath(result.Path, prefix)
	return result, err
}
func (composite *Composite) Delete(ctx context.Context, value string) (DeleteResult, error) {
	backend, inner, prefix := composite.selectBackend(value)
	result, err := backend.Delete(ctx, inner)
	result.Path = remapPath(result.Path, prefix)
	return result, err
}

func (composite *Composite) Glob(ctx context.Context, pattern, base string) (GlobResult, error) {
	if base != "" && base != "/" {
		backend, inner, prefix := composite.selectBackend(base)
		result, err := backend.Glob(ctx, pattern, inner)
		for index := range result.Matches {
			result.Matches[index].Path = remapPath(strings.TrimSuffix(result.Matches[index].Path, "/"), prefix)
			if result.Matches[index].IsDir {
				result.Matches[index].Path += "/"
			}
		}
		return result, err
	}
	result, err := composite.defaultBackend.Glob(ctx, pattern, "/")
	if err != nil {
		return GlobResult{}, err
	}
	for _, route := range composite.routes {
		innerPattern := stripPatternPrefix(pattern, route.prefix)
		routed, err := route.backend.Glob(ctx, innerPattern, "/")
		if err != nil {
			return GlobResult{}, err
		}
		for _, item := range routed.Matches {
			item.Path = remapPath(strings.TrimSuffix(item.Path, "/"), route.prefix)
			if item.IsDir {
				item.Path += "/"
			}
			result.Matches = append(result.Matches, item)
		}
		result.Truncated = result.Truncated || routed.Truncated
	}
	sort.Slice(result.Matches, func(i, j int) bool { return result.Matches[i].Path < result.Matches[j].Path })
	return result, nil
}

func (composite *Composite) Grep(ctx context.Context, pattern string, options GrepOptions) (GrepResult, error) {
	if options.Path != "" && options.Path != "/" {
		backend, inner, prefix := composite.selectBackend(options.Path)
		options.Path = inner
		result, err := backend.Grep(ctx, pattern, options)
		for index := range result.Matches {
			result.Matches[index].Path = remapPath(result.Matches[index].Path, prefix)
		}
		return result, err
	}
	remaining := options.MaxCount
	result, err := composite.defaultBackend.Grep(ctx, pattern, options)
	if err != nil {
		return GrepResult{}, err
	}
	if remaining > 0 {
		remaining -= len(result.Matches)
	}
	for _, route := range composite.routes {
		if options.MaxCount > 0 && remaining <= 0 {
			result.Truncated = true
			break
		}
		routedOptions := options
		routedOptions.Path = "/"
		routedOptions.Glob = stripPatternPrefix(options.Glob, route.prefix)
		if options.MaxCount > 0 {
			routedOptions.MaxCount = remaining
		}
		routed, err := route.backend.Grep(ctx, pattern, routedOptions)
		if err != nil {
			return GrepResult{}, err
		}
		for _, item := range routed.Matches {
			item.Path = remapPath(item.Path, route.prefix)
			result.Matches = append(result.Matches, item)
		}
		remaining -= len(routed.Matches)
		result.Truncated = result.Truncated || routed.Truncated
	}
	sort.Slice(result.Matches, func(i, j int) bool {
		if result.Matches[i].Path != result.Matches[j].Path {
			return result.Matches[i].Path < result.Matches[j].Path
		}
		return result.Matches[i].Line < result.Matches[j].Line
	})
	return result, nil
}

func (composite *Composite) Upload(ctx context.Context, uploads []Upload) []UploadResult {
	result := make([]UploadResult, len(uploads))
	for index, upload := range uploads {
		backend, inner, prefix := composite.selectBackend(upload.Path)
		part := backend.Upload(ctx, []Upload{{Path: inner, Content: upload.Content}})[0]
		part.Path = remapPath(part.Path, prefix)
		result[index] = part
	}
	return result
}
func (composite *Composite) Download(ctx context.Context, paths []string) []DownloadResult {
	result := make([]DownloadResult, len(paths))
	for index, value := range paths {
		backend, inner, prefix := composite.selectBackend(value)
		part := backend.Download(ctx, []string{inner})[0]
		part.Path = remapPath(part.Path, prefix)
		result[index] = part
	}
	return result
}

func stripPatternPrefix(pattern, prefix string) string {
	barePattern := strings.TrimPrefix(pattern, "/")
	barePrefix := strings.Trim(prefix, "/") + "/"
	if strings.HasPrefix(barePattern, barePrefix) {
		return strings.TrimPrefix(barePattern, barePrefix)
	}
	return pattern
}
