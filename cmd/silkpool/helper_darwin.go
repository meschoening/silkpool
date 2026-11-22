package main

import "github.com/meschoening/silkpool/internal/slot"

func init() {
	// The launcher each slot's launchd job runs as the service account,
	// which moves the runner's JIT config into the listener's
	// environment (ADR 0006).
	helper = slot.Helper
}
