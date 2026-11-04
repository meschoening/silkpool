# Install a Hub on Linux

The Hub is the service your nodes connect to. It holds your pool's settings and its GitHub connections, shows each node's state, relays your commands, and gets each runner its registration from GitHub. You run one Hub, on a Linux machine that every node can reach over HTTPS. That machine can be a node too.

## Install it

In a terminal on the Hub's machine, run the install command of the release you want, with `--hub`:

```sh
curl -fsSL https://github.com/meschoening/silkpool/releases/download/v<version>/install.sh | sudo sh -s -- --hub
```

`install.sh` picks the `silkpool-hub` package for the machine, downloads it from that release, checks it against the release's `SHA256SUMS`, and installs it with apt-get, dnf or yum. You can also download the package and install it yourself.

Installing doesn't start the Hub. The package puts these on the machine:

| | |
|---|---|
| `/usr/bin/silkpool-hub` | the Hub's CLI, and its service |
| `/usr/lib/systemd/system/silkpool-hub.service` | the Hub's service |
| `/usr/lib/sysusers.d/silkpool-hub.conf` | the Hub's system user, `silkpool-hub`, which the package makes |

The service runs as `silkpool-hub`, never as root. Once the Hub is set up, it keeps:

- its database, its GitHub Apps' key files and its self-signed certificate in `/var/lib/silkpool-hub`, which only `silkpool-hub` can open;
- its CLI's socket at `/run/silkpool-hub/hub.sock`, which only root can use;
- its log in the journal: `journalctl -u silkpool-hub`.

Every `silkpool-hub` command runs as root, with `sudo`.

## Set it up

`silkpool-hub init` sets the Hub up and starts it. Give it the URL your nodes and your browser will reach it at, which must be `https://`:

```sh
sudo silkpool-hub init --url https://hub.example.com
```

It prints a setup link. Open it in your browser to finish setting the Hub up, which connects it to GitHub. The link works until you sign in, or for 24 hours. For a fresh one, run `sudo silkpool-hub setup-code`.

The Hub serves your nodes and its dashboard on one listener, in one of three ways.

### Self-signed (the default)

```sh
sudo silkpool-hub init --url https://hub.example.com
```

`init` makes a key and a certificate for the URL's host in `/var/lib/silkpool-hub`, and the Hub serves them on port 443, or on the URL's port if it names one. Enrollment codes carry the Hub's key, so nodes check the Hub against it.

Your browser warns about this certificate. That's expected with a self-signed certificate, and it doesn't affect nodes, which check the Hub by its key alone. `init` prints the certificate's SHA-256 fingerprint: when your browser warns, open the certificate it shows, and go on only if its SHA-256 fingerprint is the one `init` printed.

To stop the warnings, give the Hub a certificate your browsers already trust, as certificate files or behind a proxy, in one of the next two ways. On a Mac, trusting the self-signed certificate in Keychain Access doesn't stop the warning in Safari, or in anything else that checks certificates through macOS: the certificate is valid for 10 years, and macOS refuses a TLS server certificate valid for more than 825 days, even one you trust.

### Certificate files

```sh
sudo silkpool-hub init --url https://hub.example.com --cert /etc/letsencrypt/live/hub.example.com/fullchain.pem --key /etc/letsencrypt/live/hub.example.com/privkey.pem
```

The Hub serves a certificate and key you keep up to date, from `tailscale cert`, certbot or your own certificate authority. systemd reads the files as root and hands them to the Hub, so they can stay root's. When either file changes, the Hub restarts and serves the new ones, so a renewal needs no hook. Nodes check the Hub through the machine's trusted certificates.

### Behind a proxy

```sh
sudo silkpool-hub init --url https://hub.example.com --behind-proxy
```

The Hub listens on plain HTTP on `127.0.0.1:8780`, and your proxy, such as Caddy or `tailscale serve`, serves the URL with a publicly trusted certificate and passes the traffic on. The proxy must carry WebSockets, which nodes connect with. Nodes check the Hub through the machine's trusted certificates.

`--listen <address:port>` changes where the Hub listens, in any mode. Behind a proxy, it must be a loopback address.

The Hub can listen on port 443 without running as root: its service holds only the capability to bind low ports. Don't `setcap` the binary as well, which would take that capability away.

## Its URL and its key

- **To change the Hub's URL,** run `sudo silkpool-hub set-url <url>`. It prints the nodes to re-point, and on each of them, an administrator runs `sudo silkpool set-hub-url <url>`. The Hub never moves its nodes itself.
- **Switching between the self-signed certificate and a trusted one, or losing the Hub's key,** means enrolling every node again: on each node, `sudo silkpool remove --force`, then `sudo silkpool enroll <code>` with a new code.

## The Hub's machine

The Hub keeps its GitHub Apps' private keys in its state folder, which only its own account can open. A job on a node that shares the Hub's machine runs in a slot that can't see that folder. What isn't defended is a local privilege-escalation exploit on that machine: one that reaches root reaches the Apps' keys.

A machine that's both the Hub and a node installs both packages, `silkpool-hub` and `silkpool`, which share nothing. See [Install a node on Linux](install-node-linux.md).

The Hub keeps 90 days of history by default.

## Upgrade it

Upgrade the Hub before its nodes. First copy its state folder, with the service stopped:

```sh
sudo systemctl stop silkpool-hub
sudo cp -a /var/lib/silkpool-hub /var/lib/silkpool-hub.before-upgrade
sudo systemctl start silkpool-hub
```

The Hub can't go back a version, so that copy is the way back. Restoring it loses everything since, and works only until you upgrade a node.

Then run the install command of the newer release, with `--hub`:

```sh
curl -fsSL https://github.com/meschoening/silkpool/releases/download/v<newer version>/install.sh | sudo sh -s -- --hub
```

The package restarts the running Hub, and nodes reconnect by themselves. It refuses to install an older version over a newer one.

## Uninstall it

```sh
sudo apt-get remove silkpool-hub
```

Removing the package stops and disables the Hub, and deletes its program and its service, but keeps `/var/lib/silkpool-hub`: it's the only copy of your GitHub Apps' keys and your pool. The package won't install an older version over that folder, since the Hub can't go back a version. To delete that folder too, with the units `init` wrote for certificate files, purge the package:

```sh
sudo apt-get purge silkpool-hub
```

With an `.rpm`, `sudo dnf remove silkpool-hub` removes the package and keeps the folder, and there's no purge: delete `/var/lib/silkpool-hub` yourself.

The `silkpool-hub` system user stays, as system users do.

## Install it again

When you install the package again over the `/var/lib/silkpool-hub` a removal kept, the package doesn't start the Hub, and `init` refuses, since the Hub is set up already. Start it yourself:

```sh
sudo systemctl enable --now silkpool-hub
```

If the Hub serves certificate files, the removal also disabled the unit that restarts it when they change. Start that unit with it:

```sh
sudo systemctl enable --now silkpool-hub-certs.path silkpool-hub
```

The Hub comes back with its pool, its GitHub connections and its key, and its nodes reconnect by themselves.
