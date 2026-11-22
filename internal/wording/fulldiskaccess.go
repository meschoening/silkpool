package wording

// FullDiskAccess is node.NoFullDiskAccess's detail: why a Mac's agent
// needs Full Disk Access and how the operator gives it, as decided on
// "A Mac slot's clean can't empty trustd's TemporaryItems in the
// account's /var/folders" (#306). The package installs the agent as
// /usr/local/bin/silkpool, which System Settings lists as silkpool.
const FullDiskAccess = "Without it, macOS won't let Silkpool clear what jobs leave in its service account's temporary folders, so the slots can't be reset between jobs. " +
	"In System Settings ▸ Privacy & Security ▸ Full Disk Access, click +, press ⌘⇧G, enter /usr/local/bin/silkpool, click Open, and make sure silkpool is turned on."
