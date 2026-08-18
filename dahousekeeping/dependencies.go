package dahousekeeping

import (
	"context"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	maxDependencyFloors = 512
	maxModuleNameBytes  = 500
	maxVersionBytes     = 256
)

// DependencyFloor declares the minimum acceptable version of one Go module.
type DependencyFloor struct {
	Module  string `json:"module"`
	Minimum string `json:"minimum"`
}

// DependencyStatus describes one floor comparison.
type DependencyStatus string

const (
	DependencySatisfied   DependencyStatus = "satisfied"
	DependencyStale       DependencyStatus = "stale"
	DependencyUnavailable DependencyStatus = "unavailable"
)

// DependencyResult is a diagnostic-safe comparison. Local replacement paths
// are never included.
type DependencyResult struct {
	Module    string           `json:"module"`
	Minimum   string           `json:"minimum"`
	Installed string           `json:"installed,omitempty"`
	Status    DependencyStatus `json:"status"`
}

// DependencyReport is the stable, offline result of a floor check.
type DependencyReport struct {
	Version    int                `json:"version"`
	Results    []DependencyResult `json:"results"`
	Stale      int                `json:"stale"`
	Unresolved int                `json:"unresolved"`
}

// DependencyFloorChecker compares immutable Go build metadata with an explicit
// application-owned floor list. It performs no package discovery or network I/O.
type DependencyFloorChecker struct {
	info   debug.BuildInfo
	floors []DependencyFloor
}

// NewDependencyFloorChecker compiles a checker. Build metadata and floors are
// mandatory positional inputs. Invalid static floors panic.
func NewDependencyFloorChecker(info debug.BuildInfo, floors []DependencyFloor) *DependencyFloorChecker {
	if len(floors) > maxDependencyFloors {
		panic("dahousekeeping: too many dependency floors")
	}
	seen := make(map[string]struct{}, len(floors))
	compiled := make([]DependencyFloor, len(floors))
	for index, floor := range floors {
		floor.Module = strings.TrimSpace(floor.Module)
		floor.Minimum = strings.TrimSpace(floor.Minimum)
		if floor.Module == "" || len(floor.Module) > maxModuleNameBytes || strings.ContainsAny(floor.Module, "\x00\r\n\t ") {
			panic("dahousekeeping: invalid dependency module")
		}
		if _, exists := seen[floor.Module]; exists {
			panic("dahousekeeping: duplicate dependency floor")
		}
		if _, ok := parseSemver(floor.Minimum); !ok {
			panic("dahousekeeping: dependency minimum must be canonical semantic version")
		}
		seen[floor.Module] = struct{}{}
		compiled[index] = floor
	}
	return &DependencyFloorChecker{info: info, floors: compiled}
}

// Check compares the build graph against every floor. Missing modules and
// unversioned local replacements are unresolved rather than falsely reported as
// stale. Cancellation stops comparison and returns context.Err.
func (checker *DependencyFloorChecker) Check(ctx context.Context) (DependencyReport, error) {
	report := DependencyReport{Version: 1, Results: make([]DependencyResult, 0, len(checker.floors))}
	installed := make(map[string]string, len(checker.info.Deps)+1)
	addModuleVersion(installed, checker.info.Main)
	for _, dependency := range checker.info.Deps {
		if dependency != nil {
			addModuleVersion(installed, *dependency)
		}
	}
	for _, floor := range checker.floors {
		if err := ctx.Err(); err != nil {
			return DependencyReport{}, err
		}
		result := DependencyResult{Module: floor.Module, Minimum: floor.Minimum}
		version, exists := installed[floor.Module]
		parsed, valid := parseSemver(version)
		if !exists || !valid {
			result.Status = DependencyUnavailable
			report.Unresolved++
		} else {
			result.Installed = version
			minimum, _ := parseSemver(floor.Minimum)
			if compareSemver(parsed, minimum) < 0 {
				result.Status = DependencyStale
				report.Stale++
			} else {
				result.Status = DependencySatisfied
			}
		}
		report.Results = append(report.Results, result)
	}
	return report, nil
}

func addModuleVersion(versions map[string]string, module debug.Module) {
	version := module.Version
	if module.Replace != nil {
		// A replacement with a real version is the code that actually ran. A
		// local replacement has no comparable version and stays unresolved.
		version = module.Replace.Version
	}
	versions[module.Path] = version
}

type semanticVersion struct {
	major, minor, patch uint64
	prerelease          []string
}

func parseSemver(value string) (semanticVersion, bool) {
	if len(value) < 2 || len(value) > maxVersionBytes || value[0] != 'v' {
		return semanticVersion{}, false
	}
	value = value[1:]
	if plus := strings.IndexByte(value, '+'); plus >= 0 {
		if plus == len(value)-1 || !validIdentifiers(value[plus+1:], false) {
			return semanticVersion{}, false
		}
		value = value[:plus]
	}
	var prerelease []string
	if dash := strings.IndexByte(value, '-'); dash >= 0 {
		if dash == len(value)-1 || !validIdentifiers(value[dash+1:], true) {
			return semanticVersion{}, false
		}
		prerelease = strings.Split(value[dash+1:], ".")
		value = value[:dash]
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return semanticVersion{}, false
	}
	numbers := make([]uint64, 3)
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semanticVersion{}, false
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return semanticVersion{}, false
		}
		numbers[index] = number
	}
	return semanticVersion{major: numbers[0], minor: numbers[1], patch: numbers[2], prerelease: prerelease}, true
}

func validIdentifiers(value string, numericNoLeadingZero bool) bool {
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		numeric := true
		for _, character := range identifier {
			if !((character >= '0' && character <= '9') || (character >= 'A' && character <= 'Z') ||
				(character >= 'a' && character <= 'z') || character == '-') {
				return false
			}
			if character < '0' || character > '9' {
				numeric = false
			}
		}
		if numericNoLeadingZero && numeric && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func compareSemver(left, right semanticVersion) int {
	for _, pair := range [][2]uint64{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(left.prerelease) == 0 && len(right.prerelease) > 0 {
		return 1
	}
	if len(right.prerelease) == 0 && len(left.prerelease) > 0 {
		return -1
	}
	for index := 0; index < min(len(left.prerelease), len(right.prerelease)); index++ {
		leftID, rightID := left.prerelease[index], right.prerelease[index]
		if leftID == rightID {
			continue
		}
		leftNumber, leftNumeric := numericIdentifier(leftID)
		rightNumber, rightNumeric := numericIdentifier(rightID)
		switch {
		case leftNumeric && rightNumeric:
			if len(leftNumber) < len(rightNumber) || (len(leftNumber) == len(rightNumber) && leftNumber < rightNumber) {
				return -1
			}
			return 1
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		case leftID < rightID:
			return -1
		default:
			return 1
		}
	}
	return sign(len(left.prerelease) - len(right.prerelease))
}

func numericIdentifier(value string) (string, bool) {
	for _, character := range value {
		if character < '0' || character > '9' {
			return "", false
		}
	}
	return value, true
}

func sign(value int) int {
	if value < 0 {
		return -1
	}
	if value > 0 {
		return 1
	}
	return 0
}

func (status DependencyStatus) String() string {
	return string(status)
}

func (floor DependencyFloor) String() string {
	return fmt.Sprintf("%s >= %s", floor.Module, floor.Minimum)
}
