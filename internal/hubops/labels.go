package hubops

import (
	"regexp"
	"slices"
	"strings"
)

// The label rules of ADR 0003 and "Define the registration scope model
// and labels".
// Labels are kept lowercase, since GitHub matches them ignoring case.

// labelPattern is what a label may be: lowercase letters, digits, dots,
// dashes and underscores, starting with a letter or digit.
var labelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// osLabels are the OS and architecture words, which Silkpool gives every
// runner itself, and which no other label may be.
var osLabels = []string{"linux", "windows", "macos", "x64", "arm64", "x86", "arm"}

// hostedPrefixes start GitHub-hosted runners' labels, such as
// ubuntu-latest and macos-15, which the pool label can't be.
var hostedPrefixes = []string{"ubuntu-", "windows-", "macos-"}

// normal returns label as the Hub keeps it.
func normal(label string) string { return strings.ToLower(strings.TrimSpace(label)) }

// checkWord refuses a label that isn't one.
func checkWord(label string) *Refusal {
	if !labelPattern.MatchString(label) {
		return refuse("invalid-label", "%q isn't a label: use lowercase letters, digits, dots, dashes and underscores, starting with a letter or digit", label)
	}
	if slices.Contains(osLabels, label) {
		return refuse("os-label", "the label %s is an OS or architecture label, which Silkpool gives every runner itself", label)
	}
	return nil
}

// CheckLabel refuses a custom label, as the Hub keeps it, that a node
// can't have beside the pool label. self-hosted is allowed: it's the
// escape hatch that lets generic self-hosted jobs reach the node.
func CheckLabel(label, poolLabel string) error {
	if r := checkWord(label); r != nil {
		return r
	}
	if label == poolLabel {
		return refuse("pool-label", "the label %s is the pool label, which every runner has already", label)
	}
	return nil
}

// CheckPoolLabel refuses a pool label, as the Hub keeps it, that would
// let other jobs reach the pool, or the pool's jobs other runners.
func CheckPoolLabel(label string) error {
	if r := checkWord(label); r != nil {
		return r
	}
	if label == "self-hosted" {
		return refuse("self-hosted", "the pool label can't be self-hosted: generic self-hosted jobs would reach every node")
	}
	for _, p := range hostedPrefixes {
		if strings.HasPrefix(label, p) {
			return refuse("hosted-label", "the label %s is a GitHub-hosted runner's label, so the pool label can't be it", label)
		}
	}
	return nil
}
