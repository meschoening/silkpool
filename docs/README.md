# Silkpool operator docs

Silkpool puts spare computers on duty as self-hosted GitHub Actions runners, and takes them off duty without interrupting builds. You run one **Hub**, and enroll each machine in it as a **node**. Together, its nodes are the Hub's **pool**.

- [Install a Hub on Linux](install-hub-linux.md)
- [Install a node on Linux](install-node-linux.md)

## Supported systems

Silkpool is tested on what its reference fleet runs, and later versions. On Linux, that's **Ubuntu 26.04 LTS**, for the Hub and for nodes. The Hub is supported on Linux only.

Everything else is untested, including other Linux distributions and the `.rpm` packages. `install.sh` doesn't refuse an untested system, but it says it's untested.

The Linux packages are for x86_64 (amd64) and arm64 machines: a `.deb` for Debian-based distributions, installed with apt-get, and an `.rpm` for Fedora- and RHEL-based ones, installed with dnf or yum.

## Releases

Every release is at [github.com/meschoening/silkpool/releases](https://github.com/meschoening/silkpool/releases), with:

- the packages for each system;
- `install.sh`, which downloads that release's package for the machine, checks it against the release's `SHA256SUMS` and installs it;
- `SHA256SUMS`, which lists every file's SHA-256.

The packages and `install.sh` come over HTTPS from GitHub and are checked against `SHA256SUMS`. The Linux packages aren't signed.

Every program in a release carries the release's version. To install one, use the `install.sh` of that release: its URL names the version, as in `https://github.com/meschoening/silkpool/releases/download/v<version>/install.sh`.

## Upgrading

An upgrade is the same install command for the newer release, without an enrollment code. It replaces the programs and restarts only the Hub's or the agent's service, so it never interrupts a job.

- **Upgrade the Hub first, then its nodes.** The Hub accepts nodes at its own protocol version and the one before it, and a release raises the protocol version by at most one. A node two versions behind shows Degraded, "Agent and Hub versions don't match", until it's upgraded.
- **Copy the state folder before you upgrade.** Neither the Hub nor a node can go back a version: a newer version changes its database, and the packages refuse to downgrade. The pages for the Hub and for a node say which folder to copy, and what the copy is for.
