# Windows nodes: Tailscale's unattended mode

If a Windows node reaches its Hub over Tailscale, turn on Tailscale's **Run unattended** on that machine. Without it, Tailscale on Windows runs only while someone is signed in. After a restart, such as Windows Update's, the machine stays off the tailnet until someone signs in, and the node can't reach its Hub until then. Silkpool doesn't change Tailscale's settings, and doesn't yet warn about it.

This only matters when the node's way to the Hub goes through its own Tailscale: a Hub URL with a `100.x.y.z` address or a MagicDNS name, a Hub behind `tailscale serve`, or a Hub on another network that the node reaches through a Tailscale subnet router. A Hub the node reaches without Tailscale isn't affected. On Linux, Tailscale runs whether or not anyone is signed in, so Linux nodes aren't affected either.

## The symptom

A Windows node doesn't connect to its Hub after a restart, and connects within a couple of minutes of someone signing in to the machine:

- On the Hub's dashboard, the node is greyed out, showing its last report, and its page reads "Not connected since" the restart, or a little before it.
- The agent's log, `C:\ProgramData\Silkpool\logs\agent.log`, repeats `couldn't connect to the Hub` from the boot until the sign-in, retried with a growing wait. Each attempt usually times out dialing the Hub's tailnet address:

  ```
  level=WARN msg="couldn't connect to the Hub" err="... dial tcp 100.x.y.z:8443: connectex: A connection attempt failed because the connected party did not properly respond after a period of time, ..."
  ```

- Once someone signs in, the next attempt connects, and the node carries on as usual.

The agent itself is fine. Its service starts at boot, without anyone signed in, and keeps its local rules: a pause's auto-resume still comes at its time. But without the Hub it can't get a runner, so it serves no jobs.

## Why it happens

By default, Tailscale on Windows runs in the signed-in user's session. When nobody is signed in, including after a restart, it isn't connected, and the machine can't reach any tailnet address. Silkpool's agent runs as a Windows service, so it does start at boot, but its connection to a Hub on the tailnet has nowhere to go.

Tailscale's **unattended mode** keeps Tailscale running with nobody signed in, and across restarts ([Tailscale: Run unattended](https://tailscale.com/kb/1088/run-unattended)). Tailscale also calls it server mode.

Signing in to Windows automatically at boot would work around it too, but it leaves the machine signed in to a desktop that anyone at it can use. Unattended mode doesn't need that.

## What to set

Do this on each Windows node whose Hub is on the tailnet, once Tailscale is installed and connected. It's a setting of Tailscale's, so it doesn't matter whether Silkpool is installed yet.

In the Tailscale app, in the system tray:

1. Right-click the **Tailscale** icon.
2. Hover over **Preferences**.
3. Select **Run unattended**.
4. When Tailscale asks, select **Yes**.

Or from the command line, in a PowerShell:

```powershell
& 'C:\Program Files\Tailscale\tailscale.exe' set --unattended=true
```

Unattended mode belongs to the Windows account that turned it on. That account owns Tailscale's settings on the machine, and other accounts can't change them or switch Tailscale accounts until it's turned off again ([Tailscale: Run unattended](https://tailscale.com/kb/1088/run-unattended)). Turn it on from the account that looks after the machine. To turn it off, clear the same menu item, or run `tailscale set --unattended=false`.

If your organisation manages Tailscale with Group Policy or another tool, the `UnattendedMode` policy can set it for you, or keep you from changing it. Ask whoever manages it.

## How to check

In the Tailscale app, **Run unattended** under **Preferences** has a check mark when it's on.

Or from the command line:

```powershell
& 'C:\Program Files\Tailscale\tailscale.exe' debug prefs
```

With unattended mode on, the output has `"ForceDaemon": true`. With it off, there's no `ForceDaemon` line at all.

To check the whole path, restart the machine and don't sign in. After a few minutes, the node should no longer be greyed out on the Hub's dashboard, and its page should no longer read "Not connected since". If it doesn't, check in `services.msc` that the **Tailscale** service is set to start **Automatic** and is **Running**. From another machine on the tailnet, `tailscale ping` and the node's name or address tells you whether the machine is on the tailnet at all.
