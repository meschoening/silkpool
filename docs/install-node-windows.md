# Install a node on Windows

A node is a machine enrolled in your Hub, whose slots run your GitHub Actions jobs. Each slot runs one job at a time, and every slot runs as one dedicated account, Silkpool's service account. On Windows, a node has a tray app in the notification area for the machine's administrators, beside the `silkpool` CLI and the Hub's dashboard.

## Before you install

- **Microsoft Defender's exclusions:** Defender can mistake Silkpool's programs, which aren't signed, for malware and quarantine them, which breaks the node. Installing a Windows node includes a step for that: before the install command, exclude Silkpool's two folders from Defender's scans, as [Microsoft Defender's exclusions](windows-defender.md#before-you-install-add-the-exclusions) gives. **That weakens Defender's protection of the machine,** and of what your jobs check out, build and run there: read [what the exclusions cost](windows-defender.md#what-the-exclusions-cost) first.
- **PowerShell's execution policy:** on a Windows client, the policy out of the box runs no scripts, so every PowerShell step of your jobs fails. Set it as [PowerShell's execution policy](windows-execution-policy.md) says, before or after the install. The install itself works under that default, run as Add node gives it.
- **Smart App Control must be off.** It blocks programs that aren't signed and that Microsoft doesn't know, GitHub's runner application among them, whether or not Silkpool is signed. While it's on, the node shows Degraded, "Smart App Control is on", and runs no jobs. Turn it off in Windows Security ▸ App & browser control.
- **Policies that run only signed or approved programs,** such as App Control for Business or AppLocker, block Silkpool's programs and GitHub's runner application, which aren't signed. Silkpool isn't tested under such a policy. On a machine your organisation manages, ask whoever manages it.
- **Network:** the node connects out to your Hub's URL, and downloads GitHub's runner application from `github.com` and `release-assets.githubusercontent.com`. The install downloads Silkpool's release from the same two hosts. To take jobs, its runners also reach the GitHub Actions hosts that GitHub lists for [self-hosted runners](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#requirements-for-communication-with-github). The node accepts no connections in, so it works behind NAT. If it reaches its Hub over Tailscale, turn on [Tailscale's unattended mode](windows-tailscale.md), or after a restart it can't reach the Hub until someone signs in.
- **Clone a machine before you enroll it, never after.** An enrolled machine's copy carries the same node key, and the Hub shows "This node's key is in use on two machines" until you remove the node and enroll each copy.

## Install and enroll it

In a PowerShell run as administrator, add Defender's exclusions first:

```powershell
Add-MpPreference -ExclusionPath 'C:\Program Files\Silkpool', 'C:\ProgramData\Silkpool'
```

Then run the command the Hub's **Add node** page shows for this machine, with a new enrollment code in it, in the same PowerShell:

```powershell
& ([scriptblock]::Create((irm https://github.com/meschoening/silkpool/releases/download/v<hub version>/install.ps1))) -Code <code>
```

`install.ps1` picks the zip for the machine, x64 or ARM64, downloads it from the Hub's own release into a temporary folder, checks it against the release's `SHA256SUMS`, and runs `silkpool.exe install` from there, which installs Silkpool and starts its service. Then it waits up to 30 seconds for the service and enrolls the machine with the code. Run it as a command, as Add node gives it, not as a saved script file, so that the execution policy doesn't stop it. If Defender flags the install itself, which the exclusions don't cover, install from a folder of your own as [If Defender flags the install itself](windows-defender.md#if-defender-flags-the-install-itself) says.

To install without enrolling, leave out `-Code` and the code. Then enroll the machine later, from a new PowerShell run as administrator, with a code from the Add node page, or from `sudo silkpool-hub code new` on the Hub's machine:

```powershell
silkpool enroll <code>
```

A code works once, for 24 hours. The machine enrolls under its host name, unless you give `--name`. A new node is **Paused indefinitely**, with auto-resume off and one slot. Check its auto-resume and its slot limit, in the tray app or with the CLI, then put it on duty:

```powershell
silkpool resume
```

`silkpool status` shows what the node is doing. A PowerShell opened before the install doesn't find `silkpool` yet, so open a new one. Only the machine's administrators can use `silkpool` and the tray app, and `enroll` and `remove` need a PowerShell run as administrator.

## What it puts on the machine

| | |
|---|---|
| `C:\Program Files\Silkpool` | `silkpool.exe`, the node's CLI and its agent, and `silkpool-tray.exe`, the tray app |
| the `Silkpool` service | the agent, which runs as LocalSystem, starts at boot and is restarted if it fails. It idles until the machine is enrolled. |
| `C:\ProgramData\Silkpool` | the agent's state, in `state`; its logs, in `logs`; the runner application's installs; and each slot's folder, its runner and its temporary folder. Only SYSTEM and administrators can change it. |
| `silkpool` | Silkpool's service account, the account every slot runs as, and its profile |
| a service for each slot | `SilkpoolSlot` and the slot's number, which hosts the slot's runner as the service account |
| the system `Path` | `C:\Program Files\Silkpool`, so a new PowerShell finds `silkpool` |
| the Run entry and the Start menu's **Silkpool** | the tray app, started when an administrator signs in |
| the Apps entry **Silkpool** | its Uninstall runs `silkpool uninstall` |

Before its slots get runners, Silkpool also adds entries that deny its service account changing the folders in Program Files that every account may change, as some installers leave them, and reading what its read check finds it could read in the places it checks, such as the entries at the top of fixed drives. Every other account keeps what it had. If the account could still change or read something the check looks at, the exposure check below says so. It doesn't look everywhere, so keep data that jobs mustn't read out of folders that every account may read. The uninstall removes the entries Silkpool added.

## What a slot can and can't do

Every slot runs its jobs as Silkpool's service account, `silkpool`: a standard local account that isn't an administrator, made when Silkpool is installed and deleted when it's uninstalled. Its password is random, and never stored. Each slot's processes run in a job object of their own.

- **Defended:** a job can't change the agent or its state. When its runner ends, nothing of the job is left running in its slot, and nothing in the slot's folders or its temporary folder carries over to the next job.
- **Not defended:** kernel or OS privilege-escalation exploits, what a job can reach on your network, and one slot's job against another slot's on the same machine. Since every slot runs as one account, a job can end another slot's processes, and may reach its files. That's accepted because every slot on a node serves the same operator.
- **Cleaned when every slot is idle:** the service account's profile, its scheduled tasks, its Recycle Bin, and any process it still has outside a slot. Until then, what a job leaves in those can reach a later job on any slot.
- **The exposure check** runs as the service account whenever a slot is prepared, before it gets a runner. If the account could still change something in the folders of the machine's software, or open something it shouldn't, every slot closes and the node shows Degraded, "Slot can see files outside its own", naming the path. Fix its permissions or move it.
- **Jobs a slot can't run:**
  - container jobs and service containers, which GitHub runs on Linux runners only;
  - anything that needs a signed-in desktop session.
- **No caches are kept on purpose.** Use GitHub's `actions/cache` instead.
- Silkpool's runners never serve public repositories.

**Emergency stop** stops the node's running jobs. GitHub marks each one failed, and the node stays Paused indefinitely until you resume it.

## The machine's resources

Slots always give way on CPU to the machine's other work: their processes run at below-normal priority. You can also give a node a **budget**: the most memory and CPU its slots may use together.

```powershell
silkpool budget --memory 4GB --cpus 2
silkpool budget --none
```

- A budget bounds memory and CPU only, never disk or IO, so a runaway job can fill the disk.
- The memory budget counts the memory the slots' processes have committed, including what Windows has paged out.
- The budget is shared by every slot, so running out can end a process in another slot's job.

## Logs and diagnostics

The agent's log is `C:\ProgramData\Silkpool\logs\agent.log`. Each runner's own logs are copied to `C:\ProgramData\Silkpool\logs\runners\<runner>\` when its job ends, and kept for 14 days, up to 1 GB in all, for an administrator to read there. The diagnostics report never includes them.

`silkpool diagnostics` prints the node's diagnostics report on the machine, which works even when the Hub can't reach the node. On Windows it also shows each PowerShell's execution policy.

## Upgrade it

Upgrade the Hub first, then its nodes. First copy the agent's state folder, with the service stopped, in a PowerShell run as administrator. The copy is a backup for troubleshooting and support. It holds the node's key, so it goes in your own profile, which other accounts can't read, and not elsewhere in `C:\ProgramData`, which every account can:

```powershell
Stop-Service Silkpool
Copy-Item -Recurse 'C:\ProgramData\Silkpool\state' "$HOME\silkpool-state.before-upgrade"
Start-Service Silkpool
```

Running jobs carry on while the agent is stopped: each slot's runner is a service of its own. Then run the install command of the Hub's new release, without `-Code`:

```powershell
& ([scriptblock]::Create((irm https://github.com/meschoening/silkpool/releases/download/v<hub version>/install.ps1)))
```

It stops and restarts only the agent's service, and running jobs carry on through the restart. The node stays enrolled. A tray app that's open starts itself again on the new version. The old programs Windows still held are deleted when it next starts.

Going back a version isn't supported: the install refuses an older version over a newer one. The copy isn't a way back either, so don't put it in place of the agent's state folder.

## Uninstall it

Remove the node from its Hub first, in a PowerShell run as administrator:

```powershell
silkpool remove
```

It waits for running jobs to finish, resets the slots, and leaves the Hub for good. If the Hub is gone, use `silkpool remove --force`, which removes the node on this machine only, then remove it in the Hub too if you can.

Then uninstall Silkpool from **Settings ▸ Apps ▸ Installed apps ▸ Silkpool**, or with `silkpool uninstall` in a PowerShell run as administrator. That deletes everything Silkpool put on the machine: the service account and its profile, the slots' services, the agent's service, the entries above, `C:\ProgramData\Silkpool` and the programs. It refuses, and changes nothing, while the machine is still enrolled, while a slot's service runs, or while a process still runs as the service account.

Then remove Defender's exclusions, as [Removing the exclusions](windows-defender.md#removing-the-exclusions) says.
