package linux

// The check gate's privileged-linux lane builds the .deb and .rpm
// packages from the commit at two versions besides the source's, for the
// packages' own test, which installs, upgrades and downgrades between
// them alone in the lane's container (docs/testing.md). It leaves each
// version's in a folder of CheckDir named for it.
const (
	CheckDir   = "/var/cache/silkpool-check"
	CheckOlder = "0.0.1-rc.1"
	CheckNewer = "0.0.1"
)
