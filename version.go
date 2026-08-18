package dago

import (
	"encoding/json"
	"runtime/debug"
	"strings"
)

const modulePath = "github.com/semistrict/dago"

// Version returns the installed module version recorded by the Go toolchain.
// Development builds that do not carry a module version report "development".
func Version() string {
	return versionFromBuildInfo(debug.ReadBuildInfo())
}

func versionFromBuildInfo(info *debug.BuildInfo, ok bool) string {
	if !ok || info == nil {
		return "development"
	}
	if info.Main.Path == modulePath {
		return usableModuleVersion(&info.Main)
	}
	for _, dependency := range info.Deps {
		if dependency != nil && dependency.Path == modulePath {
			return usableModuleVersion(dependency)
		}
	}
	return "development"
}

func usableModuleVersion(module *debug.Module) string {
	if module == nil {
		return "development"
	}
	if replacement := module.Replace; replacement != nil {
		if validModuleVersion(replacement.Version) {
			return replacement.Version
		}
		if validModuleVersion(module.Version) {
			return module.Version + "+local"
		}
		return "development"
	}
	if validModuleVersion(module.Version) {
		return module.Version
	}
	return "development"
}

func validModuleVersion(version string) bool {
	return version != "" && version != "(devel)" && !strings.EqualFold(version, "development")
}

func withVersionMetadata(metadata map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(metadata)+1)
	for key, value := range metadata {
		result[key] = append(json.RawMessage(nil), value...)
	}
	versions := map[string]string{}
	if raw := result["lc_versions"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &versions)
	}
	versions["dago"] = Version()
	encoded, err := json.Marshal(versions)
	if err != nil {
		panic(err)
	}
	result["lc_versions"] = encoded
	return result
}
