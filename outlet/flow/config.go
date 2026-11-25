// SPDX-FileCopyrightText: 2025 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package flow

import (
	"akvorado/outlet/flow/templatecache"
)

// Configuration describes the configuration for the flow component.
type Configuration struct {
	// StatePersistFile defines a file to store decoder state (templates, sampling
	// rates) to survive restarts.
	StatePersistFile string `validate:"isdefault|filepath"`
	// TemplateCache configures the shared template cache. When nil, a local
	// disabled cache is used.
	TemplateCache *templatecache.Configuration `yaml:",omitempty"`
}

// DefaultConfiguration returns the default configuration for the flow component.
func DefaultConfiguration() Configuration {
	return Configuration{}
}
