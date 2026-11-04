// Package version holds the version of this build of Silkpool's
// binaries, which the agent reports to its Hub and its administrators,
// and which the node CLI compares with the running agent's.
package version

// Version is this build's version. Releases set it with
// -ldflags "-X github.com/meschoening/silkpool/internal/version.Version=<v>".
var Version = "0.1.0-dev"
