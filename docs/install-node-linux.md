# Install a node on Linux

A node is a machine enrolled in your Hub, whose slots run your GitHub Actions jobs. Each slot runs one job at a time, as a dedicated account of its own. On Linux, a node has no desktop app: you use the `silkpool` CLI, and the Hub's dashboard.

## Before you install

- **Network:** the node connects out to your Hub's URL, and downloads GitHub's runner application from `github.com` and `release-assets.githubusercontent.com`. It accepts no connections in, so it works behind NAT.
- **Toolchains go in the OS folders,** such as `/usr` and `/opt`, installed system-wide by an administrator. A slot sees only its own folders and the OS folders, read-only, and nothing a job writes outside its own folders survives the job. `/nix` and `/snap` count as OS folders, but snap-packaged tools don't run in slots, since they need a setuid helper that slots can't use.
- **Clone a machine before you enroll it, never after.** An enrolled machine's copy carries the same node key, and the Hub shows "This node's key is in use on two machines" until you remove the node and enroll each copy.
- **Keep `/var/lib/silkpool` on one filesystem.** It holds the runner application's installs and the slots' folders, and each runner's files are hardlinks to an install.

## Install and enroll it

The Hub's **Add node** page shows the command for this machine, with a new enrollment code in it:

```sh
curl -fsSL https://github.com/meschoening/silkpool/releases/download/v<hub version>/install.sh | sudo sh -s -- <code>
```

`install.sh` picks the `silkpool` package for the machine, downloads it from the Hub's own release, checks it against the release's `SHA256SUMS`, installs it with apt-get, dnf or yum, and enrolls the machine with the code.

To install without enrolling, leave the code out. Then enroll the machine later with a code from the Add node page, or from `sudo silkpool-hub code new` on the Hub's machine:

```sh
sudo silkpool enroll <code>
```

A code works once, for 24 hours. The machine enrolls under its host name, unless you give `--name`. A new node is **Paused indefinitely**, with auto-resume off and one slot. Check its auto-resume and its slot limit, then put it on duty:

```sh
silkpool resume
```

`silkpool status` shows what the node is doing. Only the machine's administrators can use `silkpool`.

## What it puts on the machine

| | |
|---|---|
| `/usr/bin/silkpool` | the node's CLI, and its agent service |
| `/usr/lib/systemd/system/silkpool.service` | the agent's service, which the package enables and starts. It idles until the machine is enrolled. |
| `/usr/lib/systemd/system/silkpool.slice` | the slice every slot runs in |
| `/var/lib/silkpool` | the agent's state, in `state`, only root's; the runner application's installs; and each slot's folder |
| `/var/log/silkpool` | the agent's log, and each runner's logs, only root's |
| `/run/silkpool` | the CLI's socket |

The package also installs what GitHub's runner application needs: `ca-certificates`, ICU and the Kerberos libraries.

If the agent's service isn't running, start it with `sudo systemctl start silkpool`.

## What a slot can and can't do

Each slot runs its jobs as a slot account of its own: a standard account that can't log in, isn't in `sudo` or any admin group, and is deleted with the slot. On Linux, a slot's jobs also run in a systemd sandbox.

- **Defended:** a job can't read or change your files, another slot's files, the agent or the Hub. It can't see `/home`, `/root`, `/mnt`, `/media`, or any other top-level folder outside the OS folders. When its runner ends, nothing of the job is left running, and nothing in the slot carries over to the next job.
- **Not defended:** kernel or OS privilege-escalation exploits, and what a job can reach on your network. Slots get the machine's network as it is.
- **The exposure check** runs as each slot account, before the slot gets a runner. If the slot could change something in the OS folders, such as a folder in `/usr` or `/opt` that the slot account can write, the node shows Degraded, "Slot can see files outside its own", naming the path. Fix its permissions or move it.
- **Jobs a slot can't run:**
  - anything that needs Docker or another container engine: a job's `container:`, service containers, `docker build`, and builds such as cibuildwheel's manylinux images;
  - anything that needs a signed-in desktop session.
- **No caches survive a job.** A slot's home and folders are emptied after every job, so tool, package and image caches don't carry over. Use GitHub's `actions/cache` instead.
- Silkpool's runners never serve public repositories.

**Emergency stop** stops the node's running jobs. GitHub marks each one failed, and the node stays Paused indefinitely until you resume it.

## The machine's resources

Slots always give way on CPU to the machine's other work. You can also give a node a **budget**: the most memory and CPU its slots may use together.

```sh
silkpool budget --memory 4GB --cpus 2
silkpool budget --none
```

- A budget bounds memory and CPU only, never disk or IO, so a runaway job can fill the disk.
- Without a budget, slots may use the whole machine, at a lower CPU priority than its other work.
- The budget is shared by every slot, so running out can end a process in another slot's job.
- On a machine whose `/tmp` is a tmpfs, a slot's `/tmp` counts toward its memory budget.
- The budget is set through Silkpool, never by editing `silkpool.slice`. Its other settings, such as `AllowedCPUs=`, go in a drop-in: `sudo systemctl edit silkpool.slice`.

## Logs and diagnostics

The agent's log is `/var/log/silkpool/agent.log`. Each runner's own logs are copied to `/var/log/silkpool/runners/<runner>/` when its job ends, and kept for 14 days, up to 1 GB in all, for an administrator to read there. The diagnostics report never includes them.

`silkpool diagnostics` prints the node's diagnostics report on the machine, which works even when the Hub can't reach the node.

## Upgrade it

Upgrade the Hub first, then its nodes. First copy the agent's state folder, with the service stopped. The copy is a backup for troubleshooting and support:

```sh
sudo systemctl stop silkpool
sudo cp -a /var/lib/silkpool/state /var/lib/silkpool-state.before-upgrade
sudo systemctl start silkpool
```

Running jobs carry on while the agent is stopped. Then run the install command of the Hub's new release, without a code:

```sh
curl -fsSL https://github.com/meschoening/silkpool/releases/download/v<hub version>/install.sh | sudo sh
```

The package restarts the agent, and running jobs carry on through the restart.

Going back a version isn't supported: the package refuses to install an older version over a newer one. The copy isn't a way back either, so don't put it in place of the agent's state folder. The agent numbers each event it reports, and the Hub drops any whose number it has already stored, so it would drop the events the node records next.

## Uninstall it

Remove the node from its Hub first:

```sh
sudo silkpool remove
```

It waits for running jobs to finish, deletes the slots' accounts, and leaves the Hub for good. If the Hub is gone, use `sudo silkpool remove --force`, which removes the node on this machine only, then remove it in the Hub too if you can.

Then remove the package:

```sh
sudo apt-get remove silkpool
```

or `sudo dnf remove silkpool`. That deletes everything Silkpool put on the machine: the program, its units, `/var/lib/silkpool` and `/var/log/silkpool`. While the machine is still enrolled, the package refuses to be removed:

```text
This machine is still enrolled in https://hub.example.com. Run `silkpool remove` first, or `silkpool remove --force` if the Hub is gone.
```

The agent's service answers that check, so it must be running when you remove the package.
