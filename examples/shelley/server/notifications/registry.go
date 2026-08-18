package notifications

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"tailscale.com/syncs"
)

// ChannelFactory creates a Channel from a config map.
// The config map is the raw JSON object from the "notification_channels" array,
// minus the "type" key which was already used for lookup.
type ChannelFactory func(config map[string]any, logger *slog.Logger) (Channel, error)

var registry syncs.Map[string, ChannelFactory]

// Register adds a channel factory to the global registry.
// Channel implementations call this in their init() functions.
func Register(typeName string, factory ChannelFactory) {
	typeName = strings.TrimSpace(typeName)
	if typeName == "" || factory == nil {
		panic("notification type and factory are required")
	}
	if _, exists := registry.Load(typeName); exists {
		panic("duplicate notification channel type: " + typeName)
	}
	registry.Store(typeName, factory)
}

// RegisteredTypes returns the names of all registered channel types.
func RegisteredTypes() []string {
	var types []string
	for t := range registry.Keys() {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// CreateFromConfig creates a Channel from a config map by looking up
// the "type" field in the registry and calling the corresponding factory.
func CreateFromConfig(config map[string]any, logger *slog.Logger) (Channel, error) {
	typeName, ok := config["type"].(string)
	typeName = strings.TrimSpace(typeName)
	if !ok || typeName == "" {
		return nil, fmt.Errorf("notification channel config missing \"type\" field")
	}

	factory, ok := registry.Load(typeName)

	if !ok {
		return nil, fmt.Errorf("unknown notification channel type: %q", typeName)
	}

	channel, err := factory(config, logger)
	if err != nil {
		return nil, err
	}
	if nilChannel(channel) {
		return nil, fmt.Errorf("notification channel factory %q returned nil", typeName)
	}
	if strings.TrimSpace(channel.Name()) == "" {
		return nil, fmt.Errorf("notification channel factory %q returned an unnamed channel", typeName)
	}
	return channel, nil
}
