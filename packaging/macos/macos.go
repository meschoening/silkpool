// Package macos lays out the macOS node's .pkg, from "Define how each
// binary is packaged, installed, upgraded and signed per OS", recorded as
// ADR 0013, and ADR 0016: its payload, which pkgbuild takes as its root,
// and its install scripts. The release workflow's macOS job stages them
// with the stage command beside this file, then signs, builds and
// notarizes the .pkg. It also holds what sudo silkpool uninstall removes,
// since macOS has no package uninstaller.
package macos

// The bundle ID, which is the .pkg's identifier too, and the root of the
// launchd labels.
const (
	ID          = "io.github.meschoening.silkpool"
	AgentLabel  = ID + ".agent"
	TrayLabel   = ID + ".tray"
	Arch        = "arm64" // Apple silicon only: there are no Intel Macs to test on
	AppName     = "Silkpool"
	appBinary   = AppName // the app's executable, in Contents/MacOS
	appBundleID = ID
)

// What the .pkg installs, by its path on the Mac.
const (
	Binary      = "/usr/local/bin/silkpool"
	App         = "/Applications/" + AppName + ".app"
	AppBinary   = App + "/Contents/MacOS/" + appBinary
	AgentPlist  = "/Library/LaunchDaemons/" + AgentLabel + ".plist"
	TrayPlist   = "/Library/LaunchAgents/" + TrayLabel + ".plist"
	AgentDomain = "system"
)

// Folders are what the agent makes as it runs, which an uninstall
// deletes with what the .pkg installed: Silkpool's folder in /var/db,
// holding its state and the slots', its log folder, and its control
// socket's folder.
var Folders = []string{SilkpoolFolder, "/Library/Logs/Silkpool", "/var/run/silkpool"}

// SilkpoolFolder is Silkpool's folder in /var/db, which holds the
// service account's home and the slots' folders, where the account may
// have locked what it owned with the user flags uchg and uappnd.
const SilkpoolFolder = "/var/db/silkpool"

// FileName is the .pkg's file name for the release at version, as the
// release workflow writes it, such as silkpool_1.2.3_arm64.pkg.
func FileName(version string) string {
	return "silkpool_" + version + "_" + Arch + ".pkg"
}
