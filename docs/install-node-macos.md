# Install a node on macOS

A node is a machine enrolled in your Hub, whose slots run your GitHub Actions jobs. Each slot runs one job at a time, and every slot runs as one dedicated account, Silkpool's service account. On a Mac, a node has a tray app in the menu bar for the Mac's administrators, beside the `silkpool` CLI and the Hub's dashboard.

## Before you install

- **A Mac with Apple silicon.** Silkpool's macOS package is for Apple silicon only. Intel Macs aren't supported, and `install.sh` refuses them.
- **Full Disk Access:** once Silkpool is installed, the Mac needs Full Disk Access for it, given in System Settings by someone at the Mac or over screen sharing. Until then the node shows Degraded and takes no jobs. Read [Full Disk Access](macos-full-disk-access.md) first: your jobs get it too.
- **Network:** the node connects out to your Hub's URL, and downloads GitHub's runner application from `github.com` and `release-assets.githubusercontent.com`. The install downloads Silkpool's release from the same two hosts. To take jobs, its runners also reach the GitHub Actions hosts that GitHub lists for [self-hosted runners](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#requirements-for-communication-with-github). The node accepts no connections in, so it works behind NAT.
- **Clone a machine before you enroll it, never after.** An enrolled machine's copy carries the same node key, and the Hub shows "This node's key is in use on two machines" until you remove the node and enroll each copy.
- **On a Mac your organisation manages** with an MDM, ask whoever manages it: Full Disk Access may have to come from a configuration profile.

## Install and enroll it

The Hub's **Add node** page shows the command for this machine, with a new enrollment code in it. Run it in Terminal, as an administrator:

```sh
curl -fsSL https://github.com/meschoening/silkpool/releases/download/v<hub version>/install.sh | sudo sh -s -- <code>
```

`install.sh` downloads the release's `.pkg` from the Hub's own release, checks it against the release's `SHA256SUMS`, and installs it with `installer`. The `.pkg` is signed with Silkpool's Developer ID and notarized by Apple. The install starts the agent, and the tray app in the session at the Mac's screen, if an administrator is signed in there. It starts for other administrators when they sign in. Then `install.sh` waits up to 30 seconds for the agent and enrolls the Mac with the code.

Then give Silkpool [Full Disk Access](macos-full-disk-access.md#what-to-set).

To install without enrolling, leave the code out. Then enroll the Mac later with a code from the Add node page, or from `sudo silkpool-hub code new` on the Hub's machine:

```sh
sudo silkpool enroll <code>
```

A code works once, for 24 hours. The Mac enrolls under its host name, unless you give `--name`. A new node is **Paused indefinitely**, with auto-resume off and one slot.

A node runs jobs for one **target**: the repository, or the organization's runner group, where its runners register. The Mac enrolls with the Hub's default target. The first target the Hub gets, in the last step of its setup, becomes the default, and `sudo silkpool-hub target default <target>` changes it. A node that enrolls while the Hub has no target has none, and `silkpool resume` refuses it, with "The node has no target: choose a target first." To give it one, finish the Hub's setup, or add a target on the dashboard's **Targets** page, and choose the node's target on its page there, under **Runs jobs for**, or on the Hub's machine:

```sh
sudo silkpool-hub node set-target <node> <target>
```

`sudo silkpool-hub target list` lists the Hub's targets.

Check its auto-resume and its slot limit, in the tray app or with the CLI, as [Everyday use](everyday-use.md) says, then put it on duty:

```sh
silkpool resume
```

`silkpool status` shows what the node is doing. Only the Mac's administrators can use `silkpool` and the tray app, and `enroll` and `remove` need `sudo`.

## What it puts on the machine

| | |
|---|---|
| `/usr/local/bin/silkpool` | the node's CLI, and its agent |
| `/Applications/Silkpool.app` | the tray app |
| `/Library/LaunchDaemons/io.github.meschoening.silkpool.agent.plist` | the agent's launchd job, which runs at boot and is restarted if it stops. It idles until the Mac is enrolled. |
| `/Library/LaunchAgents/io.github.meschoening.silkpool.tray.plist` | starts the tray app when an administrator signs in |
| `/var/db/silkpool` | the agent's state, in `state`, only root's; the runner application's installs; each slot's folder; and the service account's home |
| `/Library/Logs/Silkpool` | the agent's log, and each runner's logs, only root's |
| `/var/run/silkpool` | the CLI's socket |
| `_silkpool` | Silkpool's service account, the account every slot runs as |

The package's receipt is `io.github.meschoening.silkpool`. Login Items lists the agent and the tray app under Silkpool. Switching the agent off there stops it starting at boot, so the node goes offline after the next restart.

## What a slot can and can't do

Every slot runs its jobs as Silkpool's service account, `_silkpool`: a role account that can't log in, isn't an administrator, and has no password. It's made when Silkpool is installed and deleted when it's uninstalled. Each slot's jobs run as a launchd job of their own.

- **Defended:** a job can't change the agent or its state. When its runner ends, nothing of the job is left running in its slot, and nothing in the slot's folders carries over to the next job.
- **Not defended:** kernel or OS privilege-escalation exploits, what a job can reach on your network, and one slot's job against another slot's on the same Mac. Since every slot runs as one account, a job can signal another slot's processes, and may reach its files. That's accepted because every slot on a node serves the same operator.
- **Cleaned when every slot is idle:** the service account's home, its temporary folders, what it scheduled with `cron` or `at`, and any process it still has outside a slot. Until then, what a job leaves in those can reach a later job on any slot.
- **The exposure check** runs as the service account whenever a slot is prepared, before it gets a runner. If the account could change something in the folders of the Mac's software, or open something it shouldn't, every slot closes and the node shows Degraded, "Slot can see files outside its own", naming the path. Fix its permissions or move it.
- **Jobs a slot can't run:** anything that needs a signed-in desktop session.
- **No caches are kept on purpose.** Use GitHub's `actions/cache` instead.
- Silkpool's runners never serve public repositories.

**Emergency stop** stops the node's running jobs. GitHub marks each one failed, and the node stays Paused indefinitely until you resume it. [Everyday use](everyday-use.md#emergency-stop) gives it in the tray app, the CLI and the dashboard.

## The machine's resources

A Mac node has no budget: macOS has no way to bound a group of processes' memory and CPU together without breaking common toolchains. The slot limit is the only lever, so set it for what the Mac can spare.

## Logs and diagnostics

The agent's log is `/Library/Logs/Silkpool/agent.log`. Each runner's own logs are copied to `/Library/Logs/Silkpool/runners/<runner>/` when its job ends, and kept for 14 days, up to 1 GB in all, for an administrator to read there. The diagnostics report never includes them.

`silkpool diagnostics` prints the node's diagnostics report on the Mac, which works even when the Hub can't reach the node. While the node is connected, its page on the Hub's dashboard shows the same report: choose **Get its report** under Diagnostics. The Hub waits up to 30 seconds for it, and doesn't keep it.

## Upgrade it

Upgrade the Hub first, then its nodes. First copy the agent's state folder, with the agent stopped. The copy is a backup for troubleshooting and support:

```sh
sudo launchctl bootout system/io.github.meschoening.silkpool.agent
while sudo launchctl print system/io.github.meschoening.silkpool.agent >/dev/null 2>&1; do sleep 1; done
sudo cp -Rp /var/db/silkpool/state /var/db/silkpool-state.before-upgrade
sudo launchctl bootstrap system /Library/LaunchDaemons/io.github.meschoening.silkpool.agent.plist
```

`launchctl bootout` can return while launchd still stops the agent, so the loop waits until launchd has it no more. Running jobs carry on while the agent is stopped. Then run the install command of the Hub's new release, without a code:

```sh
curl -fsSL https://github.com/meschoening/silkpool/releases/download/v<hub version>/install.sh | sudo sh
```

The package restarts the agent and the tray app, and running jobs carry on through the restart. The node stays enrolled.

Going back a version isn't supported: the package refuses to install an older version over a newer one. The copy isn't a way back either, so don't put it in place of the agent's state folder.

## Uninstall it

Remove the node from its Hub first:

```sh
sudo silkpool remove
```

It waits for running jobs to finish, resets the slots, and leaves the Hub for good. If the Hub is gone, use `sudo silkpool remove --force`, which removes the node on this Mac only, then remove it in the Hub too if you can. `remove` asks before it acts, and so does `remove --force`. Where there's no terminal to ask on, as in a script, give `--yes`: `sudo silkpool remove --yes`.

macOS has no package uninstaller, so Silkpool has its own. macOS lets it delete the service account only from an app with Full Disk Access, so first give Terminal Full Disk Access, in System Settings ▸ Privacy & Security ▸ Full Disk Access, and quit and reopen it. Over SSH, turn on **Allow full disk access for remote users** in System Settings ▸ General ▸ Sharing ▸ Remote Login instead. Then:

```sh
sudo silkpool uninstall
```

It deletes everything Silkpool put on the Mac: the program, the app, the launchd jobs, the service account, `/var/db/silkpool`, `/Library/Logs/Silkpool`, `/var/run/silkpool` and the package's receipt. While the Mac is still enrolled, it refuses and changes nothing.

Once it has stopped the agent, it can still stop short, keeping the service account and the agent's state: without Full Disk Access, or while a process still runs as the service account. It says why. Fix that, start the agent again, and run it again:

```sh
sudo launchctl bootstrap system /Library/LaunchDaemons/io.github.meschoening.silkpool.agent.plist
until sudo silkpool status >/dev/null 2>&1; do sleep 1; done
sudo silkpool uninstall
```

The loop waits until the agent answers, since the uninstall asks it whether the Mac is enrolled.

It leaves Silkpool's own entry in the Full Disk Access list, which you can remove as [Full Disk Access](macos-full-disk-access.md#removing-it) says.
