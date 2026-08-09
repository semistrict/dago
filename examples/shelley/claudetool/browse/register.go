package browse

import (
	"context"

	dtool "github.com/semistrict/dago/tool"
)

// RegisterBrowserTools returns browser tools (combined browser tool + read_image) ready to be added to an agent.
// It also returns a cleanup function that should be called when done to properly close the browser.
// The browser will be initialized lazily when a browser tool is first used.
// Per-image size limits come from the native model profile in tool runtime
// context rather than from provider-specific service interfaces.
func RegisterBrowserTools(ctx context.Context) ([]dtool.Tool, func()) {
	browserTools := NewBrowseTools(ctx, 0)

	return []dtool.Tool{browserTools.NativeCombinedTool(), browserTools.NativeReadImageTool()}, func() {
		browserTools.Close()
	}
}
