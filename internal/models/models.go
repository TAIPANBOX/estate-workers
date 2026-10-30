// Package models is the closed list of Claude models a worker may run on.
//
// The list is closed on purpose: a model id the package does not know is
// refused, so a typo or a newer, pricier model never runs by accident.
package models

import (
	"fmt"
	"slices"
	"strings"
)

const (
	Haiku  = "claude-haiku-5-5"
	Sonnet = "claude-sonnet-5-5"
	Opus   = "claude-opus-5-5"
)

// Known lists every model a template or a task may name.
var Known = []string{Haiku, Sonnet, Opus}

// HardMaxRunUSD is the most one run may be allowed to spend on each model.
// A template or a config can lower it, never raise it.
var HardMaxRunUSD = map[string]float64{
	Haiku:  0.50,
	Sonnet: 2.00,
	Opus:   4.00,
}

// MinOpusReason is the shortest reason accepted for running on Opus.
const MinOpusReason = 10

// Check refuses Fable and Mythos outright, any model not in Known, and Opus
// without a written reason.
func Check(model, reason string) error {
	lower := strings.ToLower(model)
	if strings.Contains(lower, "fable") || strings.Contains(lower, "mythos") {
		return fmt.Errorf("model %q is refused: Fable and Mythos are not used by workers", model)
	}
	if !slices.Contains(Known, model) {
		return fmt.Errorf("model %q is not one of %v", model, Known)
	}
	if model == Opus && len(strings.TrimSpace(reason)) < MinOpusReason {
		return fmt.Errorf("model %q needs a reason of at least %d characters", model, MinOpusReason)
	}
	return nil
}

// DisplayName is the name used in the co-author trailer of a worker commit.
func DisplayName(model string) string {
	switch model {
	case Haiku:
		return "Claude Haiku 5.5"
	case Sonnet:
		return "Claude Sonnet 5.5"
	case Opus:
		return "Claude Opus 5.5"
	}
	return "Claude"
}
