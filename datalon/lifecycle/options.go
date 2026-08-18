package lifecycle

import (
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type preparedPolicy struct {
	kind      ArtifactKind
	root      string
	retainFor time.Duration
}

func prepareOptions(options Options) (Options, []preparedPolicy) {
	if options.CronRetention < 0 || options.MediaRetention < 0 || options.MaxCronJobs < 0 || options.MaxWalkEntries < 0 || options.MaxDepth < 0 || options.MaxReportEntries < 0 || options.MaxArtifactBytes < 0 || options.MaxSelectedBytes < 0 {
		panic("datalon lifecycle: retention and limits cannot be negative")
	}
	if options.ImmediateCronCleanup && options.CronRetention != 0 || options.ImmediateMediaCleanup && options.MediaRetention != 0 {
		panic("datalon lifecycle: immediate cleanup conflicts with a retention duration")
	}
	if options.ImmediateCronCleanup {
		options.CronRetention = 0
	} else if options.CronRetention == 0 {
		options.CronRetention = defaultCronRetention
	}
	if options.ImmediateMediaCleanup {
		options.MediaRetention = 0
	} else if options.MediaRetention == 0 {
		options.MediaRetention = defaultMediaRetention
	}
	if options.MaxCronJobs == 0 {
		options.MaxCronJobs = defaultMaxCronJobs
	}
	if options.MaxWalkEntries == 0 {
		options.MaxWalkEntries = defaultMaxWalkEntries
	}
	if options.MaxDepth == 0 {
		options.MaxDepth = defaultMaxDepth
	}
	if options.MaxReportEntries == 0 {
		options.MaxReportEntries = defaultMaxReportEntries
	}
	if options.MaxArtifactBytes == 0 {
		options.MaxArtifactBytes = defaultMaxArtifactBytes
	}
	if options.MaxSelectedBytes == 0 {
		options.MaxSelectedBytes = defaultMaxSelectedBytes
	}
	if len(options.FilePolicies) > 64 {
		panic("datalon lifecycle: too many file policies")
	}
	policies := []preparedPolicy{{kind: ArtifactInboundMedia, root: "media/inbound", retainFor: options.MediaRetention}}
	for _, policy := range options.FilePolicies {
		root := cleanRelativeRoot(policy.RelativeRoot)
		if policy.RetainFor < 0 {
			panic("datalon lifecycle: file retention cannot be negative")
		}
		slashRoot := root
		switch policy.Kind {
		case ArtifactChannel:
			if !channelArtifactRoot(slashRoot) {
				panic("datalon lifecycle: channel artifacts must stay below a channel artifacts directory")
			}
		case ArtifactSession:
			if !strings.HasPrefix(slashRoot, "channels/") || policy.Acknowledgement != SessionDeletionAcknowledgement {
				panic("datalon lifecycle: channel session cleanup requires confined path and explicit acknowledgement")
			}
		case ArtifactTracing:
			if slashRoot != "traces" && slashRoot != "tracing" && !strings.HasPrefix(slashRoot, "traces/") && !strings.HasPrefix(slashRoot, "tracing/") {
				panic("datalon lifecycle: tracing artifacts must stay below traces or tracing")
			}
		default:
			panic("datalon lifecycle: unsupported artifact kind")
		}
		policies = append(policies, preparedPolicy{kind: policy.Kind, root: root, retainFor: policy.RetainFor})
	}
	sort.Slice(policies, func(left, right int) bool { return policies[left].root < policies[right].root })
	for index := 1; index < len(policies); index++ {
		previous := policies[index-1].root
		current := policies[index].root
		if current == previous || strings.HasPrefix(current, previous+"/") {
			panic("datalon lifecycle: file policy roots cannot overlap")
		}
	}
	return options, policies
}

func channelArtifactRoot(root string) bool {
	segments := strings.Split(root, "/")
	if len(segments) < 3 || segments[0] != "channels" || segments[1] == "artifacts" {
		return false
	}
	for _, segment := range segments[2:] {
		if segment == "artifacts" {
			return true
		}
	}
	return false
}

func cleanRelativeRoot(value string) string {
	if strings.Contains(value, "\\") || strings.ContainsAny(value, ":\x00") {
		panic("datalon lifecycle: artifact roots must use portable relative paths")
	}
	value = strings.TrimSpace(value)
	cleaned := path.Clean(value)
	if cleaned == "." || path.IsAbs(cleaned) || filepath.IsAbs(value) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		panic("datalon lifecycle: artifact root must stay beneath assistant state")
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == "" || segment == "." || segment == ".." || len(segment) > 255 {
			panic("datalon lifecycle: artifact root contains an unsafe segment")
		}
	}
	return cleaned
}
