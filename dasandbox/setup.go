package dasandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/semistrict/dago/dabackend"
)

func (registry *Registry) runSetup(ctx context.Context, session *Session, script []byte) error {
	digest := sha256.Sum256(script)
	path := "/tmp/dago-setup-" + hex.EncodeToString(digest[:8]) + ".sh"
	results := session.Upload(ctx, []dabackend.Upload{{Path: path, Content: append([]byte(nil), script...)}})
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(results) != 1 || results[0].Error != "" {
		return ErrSetupFailed
	}
	defer func() {
		deleteCtx, cancel := context.WithTimeout(context.Background(), registry.closeTimeout)
		defer cancel()
		_, _ = session.Delete(deleteCtx, path)
	}()
	setupCtx, cancel := context.WithTimeout(ctx, registry.setupTimeout)
	defer cancel()
	result, err := session.Execute(setupCtx, "bash "+path, registry.setupTimeout)
	if ctxErr := setupCtx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return ErrSetupFailed
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		exit := "unknown"
		if result.ExitCode != nil {
			exit = fmt.Sprintf("%d", *result.ExitCode)
		}
		return fmt.Errorf("%w: exit code %s", ErrSetupFailed, exit)
	}
	return nil
}
