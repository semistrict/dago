package dacode

import "github.com/semistrict/dago/daconfig"

const onboardingConfigKey = "startup.onboarding"

// decideOnboardingLaunch resolves the app-neutral launch decision. force is
// reserved for an explicit command-line request; the layered configuration and
// environment are consulted next, and an unset policy falls back to the
// completion marker. Non-interactive callers never launch a modal flow.
func decideOnboardingLaunch(
	stateDirectory string,
	interactive bool,
	force bool,
	configuration daconfig.Snapshot,
	lookup func(string) (string, bool),
) (bool, []string) {
	if !interactive {
		return false, nil
	}
	if force {
		return true, nil
	}
	entries := configuration.Select(onboardingConfigKey)
	if len(entries) == 1 && entries[0].Set {
		if configured, ok := entries[0].Value.(bool); ok {
			return configured, nil
		}
		return true, []string{"invalid onboarding configuration; setup will be offered"}
	}
	return shouldRunOnboarding(stateDirectory, lookup)
}
