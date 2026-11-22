# Silkpool operator docs

Silkpool puts spare computers on duty as self-hosted GitHub Actions runners, and takes them off duty without interrupting builds. You run one **Hub**, and enroll each machine in it as a **node**. Together, its nodes are the Hub's **pool**.

- [Install a Hub on Linux](install-hub-linux.md)
- [Install a node on Linux](install-node-linux.md)
- [Install a node on macOS](install-node-macos.md)
- [Install a node on Windows](install-node-windows.md)
- [macOS nodes: Full Disk Access](macos-full-disk-access.md): Silkpool needs it to clean up between jobs, and your jobs get it too
- [Windows nodes: PowerShell's execution policy](windows-execution-policy.md): what to set so your jobs' PowerShell steps run
- [Windows nodes: Microsoft Defender's exclusions](windows-defender.md): the install step that keeps Defender from quarantining Silkpool, and what it costs
- [Windows nodes: Tailscale's unattended mode](windows-tailscale.md): a node that reaches its Hub over Tailscale needs it, or after a restart it can't reach the Hub until someone signs in

## Supported systems

Silkpool is tested on what its reference fleet runs, and later versions:

- **Linux:** **Ubuntu 26.04 LTS**, for the Hub and for nodes. The Hub is supported on Linux only.
- **macOS:** **macOS 27**, for nodes, on a Mac with Apple silicon. Intel Macs aren't supported, and `install.sh` refuses them.
- **Windows:** **Windows 11** on x64, for nodes.

Everything else is untested, including other Linux distributions, the `.rpm` packages and Windows on ARM64. `install.sh` doesn't refuse an untested Linux system, but it says it's untested.

The Linux packages are for x86_64 (amd64) and arm64 machines: a `.deb` for Debian-based distributions, installed with apt-get, and an `.rpm` for Fedora- and RHEL-based ones, installed with dnf or yum. The macOS package is a `.pkg`, installed with `installer`. The Windows node comes as a zip for x64 (amd64) and one for ARM64, which `install.ps1` installs with `silkpool install`.

## Releases

Every release is at [github.com/meschoening/silkpool/releases](https://github.com/meschoening/silkpool/releases), with:

- the packages for each system: the Linux `.deb` and `.rpm` packages, the macOS `.pkg`, and the Windows zips;
- `install.sh`, for Linux and macOS, and `install.ps1`, for Windows, each of which downloads that release's package for the machine, checks it against the release's `SHA256SUMS` and installs it;
- `SHA256SUMS`, which lists every file's SHA-256.

The packages and the install scripts come over HTTPS from GitHub, and the packages are checked against `SHA256SUMS`. Beyond that:

- **Linux:** the packages aren't signed.
- **macOS:** the `.pkg` and the programs in it are signed with Silkpool's Developer ID, and the `.pkg` is notarized by Apple.
- **Windows:** the programs aren't signed, and there are no plans to sign them. So Microsoft Defender can mistake them for malware: a Windows node's install starts by [excluding Silkpool's folders from Defender's scans](windows-defender.md). Smart App Control, and policies that run only signed or approved programs, such as App Control for Business or AppLocker, block them, and GitHub's runner application too. [Install a node on Windows](install-node-windows.md) says what to do about each.

Every program in a release carries the release's version. To install one, use the install script of that release: its URL names the version, as in `https://github.com/meschoening/silkpool/releases/download/v<version>/install.sh`.

## Upgrading

An upgrade is the same install command for the newer release, without an enrollment code. It replaces the programs and restarts only the Hub's or the agent's service, so it never interrupts a job.

- **Upgrade the Hub first, then its nodes.** The Hub accepts nodes at its own protocol version and the one before it, and a release raises the protocol version by at most one. A node two versions behind shows Degraded, "Agent and Hub versions don't match", until it's upgraded.
- **Copy the state folder before you upgrade.** Neither the Hub nor a node can go back a version: a newer version changes its database, and the packages refuse to downgrade. The pages for the Hub and for a node say which folder to copy, and what the copy is for.
