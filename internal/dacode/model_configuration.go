package dacode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/semistrict/dago/daconfig"
	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/daproviders/modelconfig"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const showDefaultModelSentinel = "__show_default_model__"

func normalizeOptionalDefaultModelFlag(arguments []string) []string {
	result := append([]string(nil), arguments...)
	for index, argument := range result {
		if argument == "--default-model" && (index+1 == len(result) || strings.HasPrefix(result[index+1], "-")) {
			result[index] = "--default-model=" + showDefaultModelSentinel
		}
	}
	return result
}

func parseModelJSONObject(name, value string) (map[string]any, error) {
	if len(value) > 64<<10 {
		return nil, fmt.Errorf("%s JSON exceeds 64 KiB", name)
	}
	decoder := json.NewDecoder(bytes.NewBufferString(value))
	decoder.UseNumber()
	var result map[string]any
	if err := decoder.Decode(&result); err != nil || result == nil {
		return nil, fmt.Errorf("%s must be a valid JSON object", name)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s must contain exactly one JSON object", name)
	}
	return result, nil
}

func runDefaultModelCommand(ctx context.Context, options cliOptions, store *daconfig.Store, output io.Writer) error {
	if store == nil {
		return errors.New("model preference store is unavailable")
	}
	preferences := modelconfig.NewPreferenceStore(store)
	if options.clearDefaultModel {
		removed, err := preferences.ClearDefault(ctx)
		if err != nil {
			return err
		}
		if removed {
			_, err = fmt.Fprintln(output, "Default model cleared.")
		} else {
			_, err = fmt.Fprintln(output, "No default model was set.")
		}
		return err
	}
	if options.defaultModel == showDefaultModelSentinel {
		value, err := preferences.Default(ctx)
		if err != nil {
			return err
		}
		if value == "" {
			_, err = fmt.Fprintln(output, "No default model set.")
		} else {
			_, err = fmt.Fprintf(output, "Default model: %s\n", unicodesecurity.RenderTerminalSafe(value))
		}
		return err
	}
	authPath, err := authStorePath("")
	if err != nil {
		return err
	}
	credentials := dacredential.NewStore(authPath, time.Now, dacredential.Options{})
	resolver := modelconfig.NewResolver(credentials, os.LookupEnv, map[string]modelconfig.Factory{}, modelconfig.Options{})
	spec, err := resolver.Parse(ctx, options.defaultModel)
	if err != nil {
		return err
	}
	if err := preferences.SetDefault(ctx, spec.String()); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Default model set to %s\n", unicodesecurity.RenderTerminalSafe(spec.String()))
	return err
}
