package dahousekeeping

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const maxMigrationEntries = 64

var defaultLegacyStateNames = []string{
	"mcp-tokens",
	"sessions.db",
	"sessions.db-wal",
	"sessions.db-shm",
	"latest_version.json",
	"update_state.json",
	"history.jsonl",
	"onboarding_complete",
}

// LegacyStateNames returns a fresh copy of the state entries moved by the
// pinned reference implementation.
func LegacyStateNames() []string {
	return append([]string(nil), defaultLegacyStateNames...)
}

// MigrationStatus describes the outcome for one legacy name.
type MigrationStatus string

const (
	MigrationMoved             MigrationStatus = "moved"
	MigrationMissing           MigrationStatus = "missing"
	MigrationDestinationExists MigrationStatus = "destination_exists"
	MigrationRejected          MigrationStatus = "rejected"
	MigrationFailed            MigrationStatus = "failed"
)

// MigrationEntry is a path-free result suitable for diagnostics. Error text is
// deliberately omitted so reports do not disclose machine paths or file data.
type MigrationEntry struct {
	Name   string          `json:"name"`
	Status MigrationStatus `json:"status"`
}

// MigrationReport is the stable result of one migration attempt.
type MigrationReport struct {
	Version  int              `json:"version"`
	Entries  []MigrationEntry `json:"entries"`
	Moved    int              `json:"moved"`
	Skipped  int              `json:"skipped"`
	Failed   int              `json:"failed"`
	Canceled bool             `json:"canceled"`
}

// StateMigrationOptions controls migration behavior. The zero value selects
// owner-only permissions for a newly created state directory.
type StateMigrationOptions struct {
	DirectoryMode fs.FileMode
}

// StateMigrator moves a closed list of direct children into a dedicated direct
// child of the same configuration directory.
type StateMigrator struct {
	configDir string
	stateName string
	names     []string
	mode      fs.FileMode
}

// NewStateMigrator validates and compiles a state migration. configDir and
// stateDir must be absolute, and stateDir must be a direct child of configDir.
// A nil names slice selects LegacyStateNames; an explicitly empty slice
// performs no work. Invalid static configuration panics.
func NewStateMigrator(configDir, stateDir string, names []string, options StateMigrationOptions) *StateMigrator {
	if !filepath.IsAbs(configDir) || filepath.Clean(configDir) != configDir {
		panic("dahousekeeping: config directory must be an absolute clean path")
	}
	if !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir {
		panic("dahousekeeping: state directory must be an absolute clean path")
	}
	relative, err := filepath.Rel(configDir, stateDir)
	if err != nil || relative == "." || filepath.Dir(relative) != "." || !validName(relative) {
		panic("dahousekeeping: state directory must be a direct child of the config directory")
	}
	if names == nil {
		names = LegacyStateNames()
	}
	if len(names) > maxMigrationEntries {
		panic("dahousekeeping: too many legacy state names")
	}
	seen := make(map[string]struct{}, len(names))
	compiled := make([]string, 0, len(names))
	for _, name := range names {
		if !validName(name) || name == relative {
			panic("dahousekeeping: legacy state names must be distinct direct-child names")
		}
		if _, exists := seen[name]; exists {
			panic("dahousekeeping: duplicate legacy state name")
		}
		seen[name] = struct{}{}
		compiled = append(compiled, name)
	}
	mode := options.DirectoryMode
	if mode == 0 {
		mode = 0o700
	}
	if mode.Perm() != mode || mode&0o077 != 0 {
		panic("dahousekeeping: state directory mode must be owner-only permissions")
	}
	return &StateMigrator{configDir: configDir, stateName: relative, names: compiled, mode: mode}
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name &&
		!strings.ContainsAny(name, `/\\\x00`)
}

// Migrate performs an idempotent, best-effort migration. It never overwrites a
// destination and never follows a legacy symlink. Per-entry I/O failures are
// reported and do not prevent other entries from being considered.
func (migrator *StateMigrator) Migrate(ctx context.Context) MigrationReport {
	entries := make([]MigrationEntry, len(migrator.names))
	for index, name := range migrator.names {
		entries[index].Name = name
	}
	failAll := func() MigrationReport {
		for index := range entries {
			entries[index].Status = MigrationFailed
		}
		return migrationReport(ctx, entries)
	}
	if ctx.Err() != nil {
		return failAll()
	}
	info, err := os.Lstat(migrator.configDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return failAll()
	}
	root, err := os.OpenRoot(migrator.configDir)
	if err != nil {
		return failAll()
	}
	defer root.Close()

	pending := make([]int, 0, len(migrator.names))
	for index, name := range migrator.names {
		if ctx.Err() != nil {
			for remainder := index; remainder < len(entries); remainder++ {
				entries[remainder].Status = MigrationFailed
			}
			return migrationReport(ctx, entries)
		}
		source, statErr := root.Lstat(name)
		if errors.Is(statErr, fs.ErrNotExist) {
			entries[index].Status = MigrationMissing
			continue
		}
		if statErr != nil {
			entries[index].Status = MigrationFailed
			continue
		}
		if source.Mode()&os.ModeSymlink != 0 || (!source.Mode().IsRegular() && !source.IsDir()) {
			entries[index].Status = MigrationRejected
			continue
		}
		destination := filepath.Join(migrator.stateName, name)
		_, statErr = root.Lstat(destination)
		switch {
		case statErr == nil:
			entries[index].Status = MigrationDestinationExists
		case !errors.Is(statErr, fs.ErrNotExist):
			entries[index].Status = MigrationFailed
		default:
			pending = append(pending, index)
		}
	}

	// A database and its WAL/SHM sidecars are one logical state object. If
	// preflight rejects any existing member, leave all existing members in place.
	sqliteIndexes := make([]int, 0, 3)
	sqliteBlocked := false
	for index, entry := range entries {
		switch entry.Name {
		case "sessions.db", "sessions.db-wal", "sessions.db-shm":
			sqliteIndexes = append(sqliteIndexes, index)
			if entry.Status != "" && entry.Status != MigrationMissing {
				sqliteBlocked = true
			}
		}
	}
	if sqliteBlocked {
		filtered := pending[:0]
		for _, index := range pending {
			if containsIndex(sqliteIndexes, index) {
				entries[index].Status = MigrationRejected
				continue
			}
			filtered = append(filtered, index)
		}
		pending = filtered
	}
	// Orphan WAL/SHM files are not useful without a database. They may continue
	// a prior partial migration only when the destination database is present.
	databasePresent := false
	for _, entry := range entries {
		if entry.Name != "sessions.db" {
			continue
		}
		databasePresent = entry.Status == "" || entry.Status == MigrationDestinationExists
		if entry.Status == MigrationMissing {
			if destination, statErr := root.Lstat(filepath.Join(migrator.stateName, entry.Name)); statErr == nil && destination.Mode().IsRegular() {
				databasePresent = true
			}
		}
	}
	if !databasePresent {
		filtered := pending[:0]
		for _, index := range pending {
			if entries[index].Name == "sessions.db-wal" || entries[index].Name == "sessions.db-shm" {
				entries[index].Status = MigrationRejected
				continue
			}
			filtered = append(filtered, index)
		}
		pending = filtered
	}
	if len(pending) == 0 {
		return migrationReport(ctx, entries)
	}
	if err := root.MkdirAll(migrator.stateName, migrator.mode); err != nil {
		for _, index := range pending {
			entries[index].Status = MigrationFailed
		}
		return migrationReport(ctx, entries)
	}
	stateInfo, err := root.Lstat(migrator.stateName)
	if err != nil || !ownerPrivateDirectory(stateInfo) || stateInfo.Mode()&os.ModeSymlink != 0 {
		for _, index := range pending {
			entries[index].Status = MigrationRejected
		}
		return migrationReport(ctx, entries)
	}

	// Move the SQLite group first and roll it back as a unit on any failure.
	sqlitePending := make([]int, 0, len(sqliteIndexes))
	for _, index := range pending {
		if containsIndex(sqliteIndexes, index) {
			sqlitePending = append(sqlitePending, index)
		}
	}
	if len(sqlitePending) > 0 {
		moved := make([]int, 0, len(sqlitePending))
		groupFailed := false
		for _, index := range sqlitePending {
			name := entries[index].Name
			if ctx.Err() != nil || root.Rename(name, filepath.Join(migrator.stateName, name)) != nil {
				groupFailed = true
				break
			}
			moved = append(moved, index)
		}
		if groupFailed {
			for movedIndex := len(moved) - 1; movedIndex >= 0; movedIndex-- {
				index := moved[movedIndex]
				name := entries[index].Name
				_ = root.Rename(filepath.Join(migrator.stateName, name), name)
			}
			for _, index := range sqlitePending {
				entries[index].Status = MigrationFailed
			}
		} else {
			for _, index := range sqlitePending {
				entries[index].Status = MigrationMoved
			}
		}
	}
	for _, index := range pending {
		if containsIndex(sqlitePending, index) {
			continue
		}
		if ctx.Err() != nil {
			entries[index].Status = MigrationFailed
			continue
		}
		name := entries[index].Name
		if err := root.Rename(name, filepath.Join(migrator.stateName, name)); err != nil {
			entries[index].Status = MigrationFailed
			continue
		}
		entries[index].Status = MigrationMoved
	}
	return migrationReport(ctx, entries)
}

func containsIndex(indexes []int, target int) bool {
	for _, index := range indexes {
		if index == target {
			return true
		}
	}
	return false
}

func migrationReport(ctx context.Context, entries []MigrationEntry) MigrationReport {
	report := MigrationReport{Version: 1, Entries: entries, Canceled: ctx.Err() != nil}
	for _, entry := range entries {
		switch entry.Status {
		case MigrationMoved:
			report.Moved++
		case MigrationMissing, MigrationDestinationExists:
			report.Skipped++
		case MigrationRejected, MigrationFailed:
			report.Failed++
		}
	}
	return report
}
