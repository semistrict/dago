package webgpu

import (
	"strings"

	"github.com/semistrict/dago/damodel"
)

const (
	// DefaultInvokeGlobal is the JavaScript function used by a zero Options value.
	DefaultInvokeGlobal = "dagoWebGPUInvoke"
	// DefaultInterruptGlobal is called on cancellation by a zero Options value.
	DefaultInterruptGlobal = "dagoWebGPUInterrupt"
)

// Options configures the stable JavaScript bridge names and model profile.
type Options struct {
	Profile         damodel.Profile
	InvokeGlobal    string
	InterruptGlobal string
}

func compileOptions(options Options) Options {
	if options.InvokeGlobal == "" {
		options.InvokeGlobal = DefaultInvokeGlobal
	}
	if options.InterruptGlobal == "" {
		options.InterruptGlobal = DefaultInterruptGlobal
	}
	if strings.TrimSpace(options.InvokeGlobal) != options.InvokeGlobal || strings.TrimSpace(options.InterruptGlobal) != options.InterruptGlobal {
		panic("WebGPU bridge names must not contain surrounding whitespace")
	}
	options.Profile = cloneProfile(options.Profile)
	return options
}

func cloneProfile(profile damodel.Profile) damodel.Profile {
	profile.ReasoningLevels = append([]string(nil), profile.ReasoningLevels...)
	if profile.SupportsImageToolMessages != nil {
		profile.SupportsImageToolMessages = new(*profile.SupportsImageToolMessages)
	}
	if profile.SupportsPDFToolMessages != nil {
		profile.SupportsPDFToolMessages = new(*profile.SupportsPDFToolMessages)
	}
	return profile
}
