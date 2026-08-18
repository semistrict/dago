package daplugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/semistrict/dago/daweb"
)

type MaterializerOptions struct {
	GitTimeout   time.Duration
	MaxGitOutput int
	MaxFiles     int
	MaxBytes     int64
}
type SecureMaterializer struct {
	web          *daweb.Client
	gitPath      string
	gitTimeout   time.Duration
	maxGitOutput int
	maxFiles     int
	maxBytes     int64
}

// NewSecureMaterializer constructs explicit bounded network/git authority.
// web is positional; nil disables direct catalog downloads. An empty gitPath
// resolves git only when a repository-backed operation is requested.
func NewSecureMaterializer(web *daweb.Client, gitPath string, options MaterializerOptions) *SecureMaterializer {
	if options.GitTimeout < 0 || options.MaxGitOutput < 0 || options.MaxFiles < 0 || options.MaxBytes < 0 {
		panic("daplugin: materializer limits cannot be negative")
	}
	if options.GitTimeout == 0 {
		options.GitTimeout = 2 * time.Minute
	}
	if options.MaxGitOutput == 0 {
		options.MaxGitOutput = 64 << 10
	}
	if options.MaxFiles == 0 {
		options.MaxFiles = 4096
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 128 << 20
	}
	return &SecureMaterializer{web: web, gitPath: gitPath, gitTimeout: options.GitTimeout, maxGitOutput: options.MaxGitOutput, maxFiles: options.MaxFiles, maxBytes: options.MaxBytes}
}

func (materializer *SecureMaterializer) Marketplace(ctx context.Context, source MarketplaceSource, cacheRoot string) (string, error) {
	if source.Type == SourceURL {
		if materializer.web == nil {
			return "", errors.New("marketplace HTTP client is unavailable")
		}
		response, err := materializer.web.Do(ctx, source.Value, daweb.Request{})
		if err != nil {
			return "", errors.New("download marketplace failed")
		}
		if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
			return "", err
		}
		directory, err := os.MkdirTemp(cacheRoot, ".market-*")
		if err != nil {
			return "", err
		}
		path := filepath.Join(directory, "marketplace.json")
		if err := os.WriteFile(path, []byte(response.Body), 0o600); err != nil {
			return "", err
		}
		market, err := LoadMarketplace(path)
		if err != nil {
			_ = os.Remove(path)
			return "", err
		}
		for _, plugin := range market.Plugins {
			if plugin.Source.Type == SourceLocal || plugin.Source.Type == SourceDirectory {
				_ = os.Remove(path)
				return "", errors.New("URL marketplace cannot contain local plugin sources")
			}
		}
		return path, nil
	}
	remote, err := marketplaceGitURL(source)
	if err != nil {
		return "", err
	}
	return materializer.clone(ctx, remote, source.Ref, cacheRoot)
}

func (materializer *SecureMaterializer) Plugin(ctx context.Context, _ Marketplace, entry MarketplaceEntry, cacheRoot string) (MaterializedPlugin, error) {
	remote := ""
	switch entry.Source.Type {
	case SourceGitHub:
		remote = "https://github.com/" + entry.Source.Repo + ".git"
	case SourceGitSubdir, SourceURL:
		remote = entry.Source.URL
	default:
		return MaterializedPlugin{}, errors.New("unsupported remote plugin source")
	}
	root, err := materializer.clone(ctx, remote, entry.Source.Ref, cacheRoot)
	if err != nil {
		return MaterializedPlugin{}, err
	}
	if entry.Source.Path == "" {
		return MaterializedPlugin{Root: root, CleanupRoot: root}, nil
	}
	usable, err := containedPath(root, entry.Source.Path)
	if err != nil {
		_ = os.RemoveAll(root)
		return MaterializedPlugin{}, err
	}
	return MaterializedPlugin{Root: usable, CleanupRoot: root}, nil
}

func marketplaceGitURL(source MarketplaceSource) (string, error) {
	switch source.Type {
	case SourceGitHub:
		return "https://github.com/" + source.Value + ".git", nil
	case SourceGit:
		return source.Value, nil
	default:
		return "", errors.New("source is not a git repository")
	}
}

func (materializer *SecureMaterializer) clone(ctx context.Context, remote, ref, cacheRoot string) (string, error) {
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return "", errors.New("git plugin sources must be credential-free HTTPS URLs")
	}
	gitPath := materializer.gitPath
	if gitPath == "" {
		gitPath, err = exec.LookPath("git")
		if err != nil {
			return "", errors.New("git is required for repository-backed plugins")
		}
	}
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return "", err
	}
	targetFile, err := os.CreateTemp(cacheRoot, ".repository-*")
	if err != nil {
		return "", err
	}
	target := targetFile.Name()
	_ = targetFile.Close()
	_ = os.Remove(target)
	temp, err := os.MkdirTemp(cacheRoot, ".clone-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	args := []string{"clone", "--depth", "1", "--no-tags", "--single-branch"}
	if ref != "" {
		if len(ref) > 256 || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, "\x00\r\n") {
			return "", errors.New("git ref is invalid")
		}
		args = append(args, "--branch", ref)
	}
	args = append(args, "--", remote, temp)
	runCtx, cancel := context.WithTimeout(ctx, materializer.gitTimeout)
	defer cancel()
	command := exec.CommandContext(runCtx, gitPath, args...)
	process, err := configureGitProcess(command)
	if err != nil {
		return "", err
	}
	defer process.close()
	command.Env = []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	output := &boundedBuffer{remaining: materializer.maxGitOutput}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		return "", errors.New("start plugin repository clone failed")
	}
	if err := process.started(command); err != nil {
		_ = command.Cancel()
		_ = command.Wait()
		return "", err
	}
	violation := make(chan error, 1)
	stopMonitor := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopMonitor:
				return
			case <-ticker.C:
				if err := treeWithinLimits(temp, materializer.maxFiles, materializer.maxBytes); err != nil {
					select {
					case violation <- err:
					default:
					}
					_ = command.Cancel()
					return
				}
			}
		}
	}()
	waitErr := command.Wait()
	close(stopMonitor)
	select {
	case limitErr := <-violation:
		return "", limitErr
	default:
	}
	if waitErr != nil {
		if runCtx.Err() != nil {
			return "", runCtx.Err()
		}
		return "", fmt.Errorf("clone plugin repository: %w", waitErr)
	}
	if err := treeWithinLimits(temp, materializer.maxFiles, materializer.maxBytes); err != nil {
		return "", err
	}
	if err := os.Rename(temp, target); err != nil {
		return "", err
	}
	return target, nil
}

func (materializer *SecureMaterializer) Cleanup(_ context.Context, path string) error {
	return os.RemoveAll(path)
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	remaining int
}

func (writer *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if writer.remaining > 0 {
		chunk := value
		if len(chunk) > writer.remaining {
			chunk = chunk[:writer.remaining]
		}
		_, _ = writer.buffer.Write(chunk)
		writer.remaining -= len(chunk)
	}
	return original, nil
}
func opaqueKey(value string) string { return fmt.Sprintf("%x", sha256Bytes([]byte(value))) }

func treeWithinLimits(root string, maxFiles int, maxBytes int64) error {
	files := 0
	var bytes int64
	return filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files++
		bytes += info.Size()
		if files > maxFiles || bytes > maxBytes {
			return errors.New("plugin repository exceeds materialization limits")
		}
		return nil
	})
}
