package fleet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var managedPaths = []string{rootPromptName, "skills", "agents", mcpConfigName, mcpSetupName}

func prepareTargetParent(target string) error {
	clean := filepath.Clean(target)
	if clean == filepath.Dir(clean) {
		return fmt.Errorf("%w: filesystem root cannot be an assistant state directory", ErrUnsafeTarget)
	}
	if info, err := os.Lstat(clean); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: target must be a real directory", ErrUnsafeTarget)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Fleet import target: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(clean), 0o700); err != nil {
		return fmt.Errorf("create Fleet target parent: %w", err)
	}
	return nil
}

func writePrivateFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create Fleet output: %w", err)
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write Fleet output: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync Fleet output: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close Fleet output: %w", err)
	}
	ok = true
	return nil
}

func commitManagedPaths(ctx context.Context, workspace, payload, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Mkdir(target, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create assistant state directory: %w", err)
	}
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: target must remain a real directory", ErrUnsafeTarget)
	}
	if err := os.Chmod(target, 0o700); err != nil {
		return fmt.Errorf("secure assistant state directory: %w", err)
	}
	for _, name := range managedPaths {
		if err := validateManagedTarget(filepath.Join(target, name), name == "skills" || name == "agents"); err != nil {
			return err
		}
	}

	backup := filepath.Join(workspace, "backup")
	if err := os.Mkdir(backup, 0o700); err != nil {
		return fmt.Errorf("create Fleet rollback directory: %w", err)
	}
	backedUp := make([]string, 0, len(managedPaths))
	installed := make([]string, 0, len(managedPaths))
	rollback := func(cause error) error {
		var rollbackErrs []error
		for index := len(installed) - 1; index >= 0; index-- {
			if err := os.RemoveAll(filepath.Join(target, installed[index])); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		for index := len(backedUp) - 1; index >= 0; index-- {
			name := backedUp[index]
			if err := os.Rename(filepath.Join(backup, name), filepath.Join(target, name)); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		return errors.Join(append([]error{cause}, rollbackErrs...)...)
	}

	for _, name := range managedPaths {
		if err := ctx.Err(); err != nil {
			return rollback(err)
		}
		destination := filepath.Join(target, name)
		if _, err := os.Lstat(destination); err == nil {
			if err := os.Rename(destination, filepath.Join(backup, name)); err != nil {
				return rollback(fmt.Errorf("back up existing Fleet output %s: %w", name, err))
			}
			backedUp = append(backedUp, name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return rollback(fmt.Errorf("inspect existing Fleet output %s: %w", name, err))
		}
		source := filepath.Join(payload, name)
		if _, err := os.Lstat(source); err == nil {
			if err := os.Rename(source, destination); err != nil {
				return rollback(fmt.Errorf("install Fleet output %s: %w", name, err))
			}
			installed = append(installed, name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return rollback(fmt.Errorf("inspect staged Fleet output %s: %w", name, err))
		}
	}
	return nil
}

func validateManagedTarget(path string, directory bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Fleet output target: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: managed path %s is a symlink", ErrUnsafeTarget, filepath.Base(path))
	}
	if directory && !info.IsDir() {
		return fmt.Errorf("%w: managed path %s must be a directory", ErrUnsafeTarget, filepath.Base(path))
	}
	if !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("%w: managed path %s must be a regular file", ErrUnsafeTarget, filepath.Base(path))
	}
	return nil
}
