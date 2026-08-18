package dacli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/semistrict/dago/damanaged"
)

const deployStateVersion = 1

const deploymentKeyField = "dago_deployment_key"

const deploymentIdentityField = "dago_deployment_id"

// ManagedDeployer is the narrow remote surface required by Deploy.
type ManagedDeployer interface {
	Endpoint() string
	ListAgents(context.Context, string) ([]damanaged.Agent, error)
	GetAgent(context.Context, string, bool) (damanaged.Agent, error)
	CreateAgent(context.Context, map[string]any, string) (damanaged.Agent, error)
	PatchAgent(context.Context, string, map[string]any) (damanaged.Agent, error)
	GetAgentDirectory(context.Context, string) (map[string]any, error)
	CommitAgentDirectory(context.Context, string, map[string]*damanaged.DirectoryFile, string) (map[string]any, error)
}

// DeployOptions controls optional reset behavior. Its zero value safely
// updates the previously pinned remote agent or creates one when absent.
type DeployOptions struct {
	Reset bool
}

// DeployResult describes one completed managed-agent upsert.
type DeployResult struct {
	AgentID  string
	Name     string
	Revision string
	Created  bool
}

// Deploy upserts one required project through one required client and stores
// its endpoint/project binding below the required private state root.
func Deploy(ctx context.Context, client ManagedDeployer, project damanaged.Project, stateRoot string, options DeployOptions) (DeployResult, error) {
	if client == nil {
		panic("managed deploy client is required")
	}
	stateRoot = strings.TrimSpace(stateRoot)
	if stateRoot == "" {
		panic("managed deploy state root is required")
	}
	if options.Reset && project.AgentID != "" {
		return DeployResult{}, errors.New("reset cannot be used while agent.json declares agent_id")
	}
	state, err := prepareDeployState(stateRoot, project.Root, client.Endpoint())
	if err != nil {
		return DeployResult{}, err
	}
	unlock, err := lockDeployState(ctx, state.path)
	if err != nil {
		return DeployResult{}, err
	}
	defer func() { _ = unlock() }()
	if err := state.load(); err != nil {
		return DeployResult{}, err
	}
	createKey, err := projectDeploymentKey(project, client.Endpoint())
	if err != nil {
		return DeployResult{}, err
	}
	projectIdentity, err := projectDeploymentIdentity(project)
	if err != nil {
		return DeployResult{}, err
	}
	if state.CreateKey != "" && project.AgentID != "" && !options.Reset {
		return DeployResult{}, errors.New("an uncertain agent creation must be reconciled before declaring agent_id")
	}
	if !options.Reset && state.ProjectKey != "" && state.ProjectKey != createKey {
		return DeployResult{}, errors.New("extras.dago_deployment_id changed; use --reset to create a new logical deployment")
	}
	pendingCreate := state.CreateKey != ""
	if !options.Reset && pendingCreate && state.CreateKey != createKey {
		return DeployResult{}, errors.New("deploy state creation identity does not match the current project")
	}
	if !options.Reset && pendingCreate && state.ProjectKey != "" && state.ProjectKey != createKey {
		return DeployResult{}, errors.New("deploy state project identity does not match its pending creation")
	}
	var remoteBinding damanaged.Agent
	if options.Reset {
		if state.CreateKey == createKey || state.AgentID != "" && state.ProjectKey == createKey {
			return DeployResult{}, errors.New("reset requires a new extras.dago_deployment_id in agent.json")
		}
		if state.CreateKey != "" {
			if err := settlePendingCreate(ctx, client, project, state); err != nil {
				return DeployResult{}, err
			}
		}
		remoteBinding, err = recoverCreatedAgent(ctx, client, createKey)
		if err != nil {
			return DeployResult{}, err
		}
		if remoteBinding != nil {
			return DeployResult{}, errors.New("reset requires a new extras.dago_deployment_id because the current identity is already bound remotely")
		}
		if err := removeRegularState(state.path); err != nil {
			return DeployResult{}, err
		}
		state.AgentID, state.Revision, state.CreateKey, state.ProjectKey, state.ProjectIdentity = "", "", "", "", ""
		pendingCreate = false
	} else {
		remoteBinding, err = recoverCreatedAgent(ctx, client, createKey)
		if err != nil {
			return DeployResult{}, err
		}
	}
	state.ProjectKey = createKey
	if project.AgentID != "" && state.AgentID != "" && project.AgentID != state.AgentID {
		return DeployResult{}, errors.New("declared agent_id conflicts with the durable deployment binding; rotate the deployment identity and reset instead")
	}
	target := project.AgentID
	declared := target != ""
	if target == "" {
		target = state.AgentID
	}
	var agent damanaged.Agent
	created := false
	recoveredCreate := false
	if declared {
		if remoteBinding != nil {
			boundID, _ := remoteBinding["id"].(string)
			if boundID != target {
				return DeployResult{}, errors.New("declared agent_id conflicts with the remote deployment binding")
			}
		}
		existing, getErr := client.GetAgent(ctx, target, false)
		if getErr != nil {
			return DeployResult{}, getErr
		}
		if existing == nil {
			return DeployResult{}, errors.New("declared agent_id did not resolve to a remote agent")
		}
		extras, _ := existing["extras"].(map[string]any)
		if boundKey, _ := extras[deploymentKeyField].(string); boundKey != createKey {
			return DeployResult{}, errors.New("declared agent_id must already carry the current remote deployment identity binding")
		}
	} else if target != "" {
		if remoteBinding != nil {
			boundID, _ := remoteBinding["id"].(string)
			if boundID != target {
				return DeployResult{}, errors.New("durable deployment state conflicts with the remote deployment binding")
			}
		}
		existing, getErr := client.GetAgent(ctx, target, false)
		if getErr != nil {
			var apiErr *damanaged.APIError
			if errors.As(getErr, &apiErr) && apiErr.Status == 404 {
				target = ""
			} else {
				return DeployResult{}, getErr
			}
		} else {
			if existing == nil {
				return DeployResult{}, errors.New("durable deployment target did not resolve to a remote agent")
			}
			extras, _ := existing["extras"].(map[string]any)
			if boundKey, _ := extras[deploymentKeyField].(string); boundKey != createKey {
				return DeployResult{}, errors.New("durable deployment target does not carry the expected remote identity binding")
			}
		}
	}
	if target != "" {
		agent, err = client.PatchAgent(ctx, target, metadataPayloadWithDeploymentKey(project, createKey))
		if err != nil {
			var apiErr *damanaged.APIError
			if declared || !errors.As(err, &apiErr) || apiErr.Status != 404 {
				return DeployResult{}, err
			}
			target = ""
		}
	}
	if target == "" {
		state.CreateKey = createKey
		agent = remoteBinding
		if agent != nil {
			recoveredCreate = true
		} else {
			if !pendingCreate {
				if err := state.savePending(projectIdentity); err != nil {
					return DeployResult{}, err
				}
			}
			agent, err = client.CreateAgent(ctx, createPayloadWithDeploymentKey(project, createKey), createKey)
			if err != nil {
				var apiErr *damanaged.APIError
				if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != 409 {
					if removeErr := removeRegularState(state.path); removeErr != nil {
						return DeployResult{}, errors.Join(err, removeErr)
					}
					return DeployResult{}, err
				}
				recovered, recoverErr := recoverCreatedAgent(ctx, client, createKey)
				if recoverErr == nil && recovered != nil {
					agent = recovered
					recoveredCreate = true
				} else {
					if recoverErr != nil {
						return DeployResult{}, errors.Join(err, fmt.Errorf("recover uncertain agent creation: %w", recoverErr))
					}
					return DeployResult{}, errors.New("an earlier agent creation has an uncertain outcome; retry later to recover it or use --reset after reconciling the remote workspace")
				}
			}
		}
		created = true
	}
	agentID, _ := agent["id"].(string)
	if agentID == "" && target != "" {
		agentID = target
	}
	if err := validateDeployValue("agent id", agentID, 256); err != nil {
		return DeployResult{}, err
	}
	revision, _ := agent["revision"].(string)
	if recoveredCreate {
		agent, err = client.PatchAgent(ctx, agentID, metadataPayloadWithDeploymentKey(project, createKey))
		if err != nil {
			return DeployResult{}, err
		}
	}
	revision, err = syncManagedDirectory(ctx, client, agentID, project.DirectoryFiles())
	if err != nil {
		return DeployResult{}, err
	}
	if revision == "" {
		revision, _ = agent["revision"].(string)
	}
	if err := state.save(agentID, revision); err != nil {
		return DeployResult{}, err
	}
	name, _ := agent["name"].(string)
	if name == "" {
		name = project.Name
	}
	return DeployResult{AgentID: agentID, Name: name, Revision: revision, Created: created}, nil
}

func projectDeploymentKey(project damanaged.Project, endpoint string) (string, error) {
	identity, err := projectDeploymentIdentity(project)
	if err != nil {
		return "", err
	}
	return deploymentKey(identity, endpoint), nil
}

func projectDeploymentIdentity(project damanaged.Project) (string, error) {
	configured, exists := project.Extras[deploymentIdentityField]
	identity, ok := configured.(string)
	if !exists || !ok || validateDeployValue("deployment identity", identity, 256) != nil {
		return "", errors.New("agent.json extras.dago_deployment_id is required and must be a stable unique identifier")
	}
	return identity, nil
}

func deploymentKey(identity, endpoint string) string {
	material, _ := json.Marshal(map[string]string{
		"endpoint": strings.TrimRight(endpoint, "/"), "identity": identity, "version": "1",
	})
	digest := sha256.Sum256(material)
	return hex.EncodeToString(digest[:16])
}

func createPayloadWithDeploymentKey(project damanaged.Project, key string) map[string]any {
	payload := project.CreatePayload()
	extras := make(map[string]any, len(project.Extras)+1)
	for name, value := range project.Extras {
		extras[name] = value
	}
	extras[deploymentKeyField] = key
	payload["extras"] = extras
	return payload
}

func metadataPayloadWithDeploymentKey(project damanaged.Project, key string) map[string]any {
	payload := project.MetadataPayload()
	extras := make(map[string]any, len(project.Extras)+1)
	for name, value := range project.Extras {
		extras[name] = value
	}
	extras[deploymentKeyField] = key
	payload["extras"] = extras
	return payload
}

func recoverCreatedAgent(ctx context.Context, client ManagedDeployer, key string) (damanaged.Agent, error) {
	agents, err := client.ListAgents(ctx, "")
	if err != nil {
		return nil, err
	}
	var match damanaged.Agent
	for _, listed := range agents {
		agentID, _ := listed["id"].(string)
		if validateDeployValue("recovery agent id", agentID, 256) != nil {
			continue
		}
		agent, err := client.GetAgent(ctx, agentID, false)
		if err != nil {
			return nil, err
		}
		extras, _ := agent["extras"].(map[string]any)
		if recoveredKey, _ := extras[deploymentKeyField].(string); recoveredKey != key {
			continue
		}
		if match != nil {
			return nil, errors.New("deployment recovery key matched multiple remote agents")
		}
		match = agent
	}
	return match, nil
}

func settlePendingCreate(ctx context.Context, client ManagedDeployer, project damanaged.Project, state *deployState) error {
	recovered, err := recoverCreatedAgent(ctx, client, state.CreateKey)
	if err != nil {
		return fmt.Errorf("reconcile prior pending creation before reset: %w", err)
	}
	if recovered != nil {
		return nil
	}
	if validateDeployValue("stored deployment identity", state.ProjectIdentity, 256) != nil || deploymentKey(state.ProjectIdentity, client.Endpoint()) != state.CreateKey {
		return errors.New("prior pending creation lacks the identity needed for safe replay")
	}
	payload := createPayloadWithDeploymentKey(project, state.CreateKey)
	extras, _ := payload["extras"].(map[string]any)
	extras[deploymentIdentityField] = state.ProjectIdentity
	if _, err := client.CreateAgent(ctx, payload, state.CreateKey); err == nil {
		return nil
	} else {
		var apiErr *damanaged.APIError
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != 409 {
			return nil
		}
		recovered, recoverErr := recoverCreatedAgent(ctx, client, state.CreateKey)
		if recoverErr != nil {
			return errors.Join(err, fmt.Errorf("reconcile prior pending creation before reset: %w", recoverErr))
		}
		if recovered == nil {
			return errors.New("prior pending creation remains uncertain after replay with its original idempotency key")
		}
		return nil
	}
}

// DryRunPayload returns the exact payload and initial directory projection
// without reading credentials or performing network or state I/O.
func DryRunPayload(project damanaged.Project) map[string]any {
	directory := make(map[string]any, len(project.DirectoryFiles()))
	for filePath, content := range project.DirectoryFiles() {
		directory[filePath] = map[string]any{"type": "file", "content": content}
	}
	return map[string]any{"agent_payload": project.CreatePayload(), "directory_files": directory}
}

func syncManagedDirectory(ctx context.Context, client ManagedDeployer, agentID string, local map[string]string) (string, error) {
	directory, err := client.GetAgentDirectory(ctx, agentID)
	if err != nil {
		var apiErr *damanaged.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 404 {
			return "", err
		}
		directory = map[string]any{}
	}
	remote := extractDirectoryFiles(directory)
	parent := extractRevision(directory)
	delta := buildDirectoryDelta(remote, local)
	if len(delta) == 0 {
		return parent, nil
	}
	commit, err := client.CommitAgentDirectory(ctx, agentID, delta, parent)
	if err != nil {
		var apiErr *damanaged.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 409 && apiErr.Status != 412 {
			return "", err
		}
		directory, err = client.GetAgentDirectory(ctx, agentID)
		if err != nil {
			return "", err
		}
		remote = extractDirectoryFiles(directory)
		parent = extractRevision(directory)
		delta = buildDirectoryDelta(remote, local)
		if len(delta) == 0 {
			return parent, nil
		}
		commit, err = client.CommitAgentDirectory(ctx, agentID, delta, parent)
		if err != nil {
			return "", err
		}
	}
	if revision := extractRevision(commit); revision != "" {
		return revision, nil
	}
	return parent, nil
}

func buildDirectoryDelta(remote map[string]any, local map[string]string) map[string]*damanaged.DirectoryFile {
	delta := map[string]*damanaged.DirectoryFile{}
	paths := make([]string, 0, len(local))
	for filePath := range local {
		paths = append(paths, filePath)
	}
	sort.Strings(paths)
	for _, filePath := range paths {
		if remoteContent(remote[filePath]) != local[filePath] {
			delta[filePath] = &damanaged.DirectoryFile{Type: "file", Content: local[filePath]}
		}
	}
	for filePath := range remote {
		if _, exists := local[filePath]; !exists && managedDirectoryPath(filePath) {
			delta[filePath] = nil
		}
	}
	return delta
}

func remoteContent(value any) string {
	entry, ok := value.(map[string]any)
	if !ok || entry["type"] != "file" {
		return ""
	}
	content, _ := entry["content"].(string)
	return content
}

func managedDirectoryPath(filePath string) bool {
	return filePath == "AGENTS.md" || filePath == "tools.json" || strings.HasPrefix(filePath, "skills/") || strings.HasPrefix(filePath, "subagents/")
}

func extractDirectoryFiles(directory map[string]any) map[string]any {
	if files, ok := directory["files"].(map[string]any); ok {
		return files
	}
	if nested, ok := directory["directory"].(map[string]any); ok {
		if files, ok := nested["files"].(map[string]any); ok {
			return files
		}
	}
	return map[string]any{}
}

func extractRevision(payload map[string]any) string {
	for _, key := range []string{"commit_hash", "revision", "hash"} {
		if value, ok := payload[key].(string); ok && value != "" {
			return value
		}
	}
	for _, key := range []string{"commit", "directory"} {
		if nested, ok := payload[key].(map[string]any); ok {
			if value := extractRevision(nested); value != "" {
				return value
			}
		}
	}
	return ""
}

type deployState struct {
	path            string
	projectRoot     string
	endpoint        string
	AgentID         string
	Revision        string
	CreateKey       string
	ProjectKey      string
	ProjectIdentity string
}

func prepareDeployState(stateRoot, projectRoot, endpoint string) (*deployState, error) {
	absoluteState, err := filepath.Abs(stateRoot)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absoluteState, 0o700); err != nil {
		return nil, fmt.Errorf("create deploy state directory: %w", err)
	}
	info, err := os.Lstat(absoluteState)
	if err != nil || info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("deploy state root must be a real directory")
	}
	if err := os.Chmod(absoluteState, 0o700); err != nil {
		return nil, fmt.Errorf("secure deploy state directory: %w", err)
	}
	material, _ := json.Marshal(map[string]string{"endpoint": strings.TrimRight(endpoint, "/"), "project_root": projectRoot})
	digest := sha256.Sum256(material)
	statePath := filepath.Join(absoluteState, hex.EncodeToString(digest[:])+".json")
	return &deployState{path: statePath, projectRoot: projectRoot, endpoint: strings.TrimRight(endpoint, "/")}, nil
}

func (state *deployState) load() error {
	fileInfo, err := os.Lstat(state.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || fileInfo.Mode()&fs.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() || fileInfo.Size() > 64<<10 {
		return errors.New("deploy state file is invalid")
	}
	data, err := os.ReadFile(state.path)
	if err != nil {
		return fmt.Errorf("read deploy state: %w", err)
	}
	var stored struct {
		Version         int    `json:"schema_version"`
		ProjectRoot     string `json:"project_root"`
		Endpoint        string `json:"endpoint"`
		AgentID         string `json:"agent_id"`
		Revision        string `json:"revision"`
		CreateKey       string `json:"create_key"`
		ProjectKey      string `json:"project_key"`
		ProjectIdentity string `json:"project_identity"`
	}
	if err := json.Unmarshal(data, &stored); err != nil || stored.Version != deployStateVersion || stored.ProjectRoot != state.projectRoot || stored.Endpoint != state.endpoint {
		return errors.New("deploy state contents are invalid")
	}
	if stored.AgentID != "" {
		if err := validateDeployValue("stored agent id", stored.AgentID, 256); err != nil {
			return err
		}
	}
	if stored.CreateKey != "" {
		decoded, err := hex.DecodeString(stored.CreateKey)
		if err != nil || len(decoded) != 16 {
			return errors.New("deploy state creation key is invalid")
		}
	}
	if stored.ProjectKey != "" {
		decoded, err := hex.DecodeString(stored.ProjectKey)
		if err != nil || len(decoded) != 16 {
			return errors.New("deploy state project key is invalid")
		}
	}
	if stored.AgentID != "" && stored.CreateKey != "" {
		return errors.New("deploy state cannot contain both an agent ID and a creation key")
	}
	if stored.CreateKey != "" && stored.ProjectKey != "" && stored.CreateKey != stored.ProjectKey {
		return errors.New("deploy state pending creation and project identities differ")
	}
	if stored.ProjectIdentity != "" {
		if err := validateDeployValue("stored deployment identity", stored.ProjectIdentity, 256); err != nil {
			return err
		}
	}
	state.AgentID, state.Revision, state.CreateKey, state.ProjectKey, state.ProjectIdentity = stored.AgentID, stored.Revision, stored.CreateKey, stored.ProjectKey, stored.ProjectIdentity
	return nil
}

func (state *deployState) savePending(projectIdentity string) error {
	state.ProjectIdentity = projectIdentity
	return state.write(map[string]any{
		"schema_version": deployStateVersion, "project_root": state.projectRoot, "endpoint": state.endpoint,
		"create_key": state.CreateKey, "project_key": state.ProjectKey, "project_identity": state.ProjectIdentity,
	})
}

func (state *deployState) save(agentID, revision string) error {
	return state.write(map[string]any{
		"schema_version": deployStateVersion, "project_root": state.projectRoot, "endpoint": state.endpoint,
		"agent_id": agentID, "revision": revision, "project_key": state.ProjectKey, "last_deployed_at": time.Now().UTC().Format(time.RFC3339),
	})
}

func (state *deployState) write(payload map[string]any) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("create deploy state temporary name: %w", err)
	}
	temporary := state.path + ".tmp-" + hex.EncodeToString(random[:])
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create deploy state: %w", err)
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write deploy state: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("flush deploy state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close deploy state: %w", err)
	}
	if err := replaceStateFile(temporary, state.path); err != nil {
		return fmt.Errorf("replace deploy state: %w", err)
	}
	ok = true
	return nil
}

func removeRegularState(statePath string) error {
	info, err := os.Lstat(statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("deploy state file is invalid")
	}
	return os.Remove(statePath)
}

func validateDeployValue(label, value string, limit int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > limit || strings.ContainsAny(value, "/\\?#\x00\r\n") {
		return fmt.Errorf("managed deploy %s is invalid", label)
	}
	return nil
}
