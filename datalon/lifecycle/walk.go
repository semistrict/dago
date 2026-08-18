package lifecycle

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"time"
)

type filePlan struct {
	kind     ArtifactKind
	path     string
	size     int64
	modified time.Time
	identity os.FileInfo
}

func (manager *Manager) planFiles(ctx context.Context, root *os.Root, now time.Time) ([]filePlan, []filePlan, []string, error) {
	plans := make([]filePlan, 0)
	observed := make([]filePlan, 0)
	directories := make([]string, 0)
	entries := 0
	for _, policy := range manager.policies {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		rootInfo, err := root.Lstat(policy.root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
			return nil, nil, nil, ErrUnsafeState
		}
		cutoff := now.Add(-policy.retainFor)
		err = fs.WalkDir(root.FS(), policy.root, func(current string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return ErrUnsafeState
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			entries++
			if entries > manager.options.MaxWalkEntries {
				return ErrLifecycleLimit
			}
			relative := strings.TrimPrefix(strings.TrimPrefix(current, policy.root), "/")
			depth := 0
			if relative != "" {
				depth = strings.Count(relative, "/") + 1
			}
			if depth > manager.options.MaxDepth {
				return ErrLifecycleLimit
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return ErrUnsafeState
			}
			if entry.IsDir() {
				if current != policy.root {
					directories = append(directories, current)
				}
				return nil
			}
			info, err := root.Lstat(current)
			if err != nil || !info.Mode().IsRegular() {
				return ErrUnsafeState
			}
			if info.Size() < 0 || info.Size() > manager.options.MaxArtifactBytes {
				return ErrLifecycleLimit
			}
			observed = append(observed, filePlan{kind: policy.kind, path: current, size: info.Size(), modified: info.ModTime(), identity: info})
			if info.ModTime().After(cutoff) {
				return nil
			}
			plans = append(plans, filePlan{kind: policy.kind, path: current, size: info.Size(), modified: info.ModTime(), identity: info})
			return nil
		})
		if err != nil {
			return nil, nil, nil, err
		}
	}
	return plans, observed, directories, nil
}

func (manager *Manager) secureState(ctx context.Context, root *os.Root, plans []filePlan, directories []string, report *Report) error {
	policyRoots := make([]string, 0, len(manager.policies))
	for _, policy := range manager.policies {
		policyRoots = append(policyRoots, policy.root)
	}
	for _, directory := range append(policyRoots, directories...) {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeState
		}
		if info.Mode().Perm() != 0o700 {
			opened, err := root.Open(directory)
			if err != nil {
				return err
			}
			openedInfo, statErr := opened.Stat()
			chmodErr := error(nil)
			valid := statErr == nil && openedInfo.IsDir()
			if valid {
				chmodErr = opened.Chmod(0o700)
			}
			closeErr := opened.Close()
			if !valid || chmodErr != nil || closeErr != nil {
				return ErrUnsafeState
			}
			report.SecuredDirectories++
		}
	}
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(plan.path)
		if err != nil || !info.Mode().IsRegular() || !os.SameFile(plan.identity, info) {
			return ErrUnsafeState
		}
		if info.Mode().Perm() != 0o600 {
			opened, err := root.Open(plan.path)
			if err != nil {
				return ErrUnsafeState
			}
			openedInfo, statErr := opened.Stat()
			chmodErr := error(nil)
			valid := statErr == nil && openedInfo.Mode().IsRegular() && os.SameFile(plan.identity, openedInfo)
			if valid {
				chmodErr = opened.Chmod(0o600)
			}
			closeErr := opened.Close()
			if !valid || chmodErr != nil || closeErr != nil {
				return ErrUnsafeState
			}
			report.SecuredFiles++
		}
	}
	return nil
}

func directoryNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
