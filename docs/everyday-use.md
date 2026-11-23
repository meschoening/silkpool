# Everyday use of a node

Once a node is enrolled and on duty, you mostly decide when it takes jobs: you pause it, resume it, let auto-resume end a pause, change its slot limit, and, when you must, stop it at once. Each of those is the same command, with the same meaning, wherever you give it:

- **the tray app,** on a Windows or macOS node: click Silkpool's icon in the notification area or the menu bar to open its panel;
- **the `silkpool` CLI,** on the node itself;
- **the Hub's dashboard,** on the node's page: open **Nodes**, then the node.

A Linux node has no tray app, so there you use the CLI and the dashboard.

Only the machine's administrators can use the tray app and the CLI. On Windows, that's a member of Administrators; on macOS, root or a member of `admin`; on Linux, root or a member of `sudo`, `wheel` or `admin`. Pausing, resuming, auto-resume, the slot limit and Emergency stop need neither a PowerShell run as administrator nor `sudo`, though `enroll`, `remove` and `set-hub-url` do. The dashboard can send a command only while the node is connected: while it isn't, its buttons are greyed out, with "Not connected, so commands can't be sent. They come back when build-pc reconnects." The tray app and the CLI talk to the node's own agent, so they work even while the Hub can't reach it.

## On duty, Paused and Paused indefinitely

A node's **intent** is what you've asked of it, and it's always one of three:

- **On duty:** it takes jobs.
- **Paused:** it takes no new jobs until its next resume time, when it goes back on duty by itself. A node can be Paused only while auto-resume is on.
- **Paused indefinitely:** it takes no new jobs until you resume it. No resume time ends it.

Pausing never interrupts a job. A paused node shows **Draining** while any slot is still occupied, by a runner finishing its job, one waiting to be retired or one still exiting, and **Drained** once every slot is free. Emergency stop, unlike a pause, cancels the running jobs.

`silkpool status` shows the node's state, what it's doing, who changed its intent last and how, its slots, its auto-resume and its Hub. `silkpool status --watch` prints it again on every change, until you press Ctrl+C. The tray panel and the node's page show the same, the tray icon's tooltip gives the node's name and state, such as "Silkpool · build-pc · Busy", and the dashboard's **Nodes** page lists every node with what it's doing.

## Pause

The **Pause** command pauses until the node's next resume time while auto-resume is on, and pauses it indefinitely while auto-resume is off. **Pause indefinitely** always pauses it indefinitely.

| | while auto-resume is on | while auto-resume is off |
|---|---|---|
| CLI | `silkpool pause` | `silkpool pause` |
| tray and dashboard | **Pause until** and the resume time, as in **Pause until tomorrow 7:30 AM** | **Pause** |

```sh
silkpool pause
```

It says how long the pause lasts, and whether jobs are still running:

```text
Paused until tomorrow 7:30 AM.
Its running jobs finish first.
```

With auto-resume off, it says `Paused until you resume it. Auto-resume is off.`

To pause until you resume it while auto-resume is on, use **Pause indefinitely** in the tray or on the dashboard, or:

```sh
silkpool pause --indefinitely
```

While a node is Paused, the tray and the dashboard still offer **Pause indefinitely**, to keep it paused past its resume time. While it's Paused indefinitely with auto-resume on, they offer **Resume at** and the next resume time, followed by **instead**, as in **Resume at tomorrow 7:30 AM instead**: that turns it back into a pause that ends at the resume time. `silkpool pause` does the same.

## Resume

**Resume** puts the node on duty at once, from either pause. The tray and the dashboard call it **Resume now**.

```sh
silkpool resume
```

```text
Resumed: build-pc takes jobs again.
```

A node with no target can't be resumed: `silkpool resume` says "The node has no target: choose a target first." The install pages say how to give it one.

Resume checks that it's based on what you saw. If the node's settings changed after the CLI read them, or after the tray or the dashboard showed them, it doesn't apply, and says what changed:

```text
Not applied: build-pc changed after revision 7, which the command was based on.
The slot limit is now 3.
Check it with `silkpool status`, then run the command again if you still want it.
```

The tray says "Not applied: build-pc changed since the panel showed it. Check it, then try again.", and the dashboard "Refused by build-pc: it changed since this page showed it. Check it, then try again." Look at the node again, then resume it again if you still want to. Raising the slot limit, editing auto-resume and loosening the budget check the same way. Pausing, Emergency stop, lowering the slot limit, tightening the budget and turning auto-resume off without changing its time, days or zone skip the check, since they never put the node to more work.

## Auto-resume

**Auto-resume** is a node's optional setting that ends a Paused intent by itself: on or off, a time of day, days of the week and a time zone. Each selected day has one **resume time**, at that time of day in that zone. Auto-resume never ends a pause indefinitely.

A new node has auto-resume off, at 8:00 AM every day, in the machine's own time zone. If Silkpool can't find the machine's time zone, it uses Central, `America/Chicago`. Times are shown in auto-resume's zone, and can name it, as in `6:45 AM CET`, when it isn't the machine's own.

To see it, and what pausing now would do:

```sh
silkpool auto-resume
```

```text
Auto-resume  On · 7:30 AM · Mon–Fri · America/Chicago
             Pausing now would resume tomorrow 7:30 AM.
```

The second line depends on the intent. On duty, it says what pausing now would do. Paused, it says when the pause ends, as in "Resumes tomorrow 7:30 AM, in 14 h 20 min." Paused indefinitely, it says "It doesn't end a pause indefinitely.", then when pausing with it on would resume. With auto-resume off, it says "Pausing now would pause until you resume it." on duty, and "Off, so a pause lasts until you resume it." while Paused indefinitely.

To change it with the CLI:

- `silkpool auto-resume on` turns it on, keeping its time, days and zone, or changing those you give.
- `silkpool auto-resume set` changes the time, days or zone you give, and leaves it on or off as it is. Give it at least one of `--at`, `--days` and `--zone`.
- `silkpool auto-resume off` turns it off. It keeps the time, days and zone for later: `Off · keeps 7:30 AM, Mon–Fri, America/Chicago for later`.

The fields are:

- `--at`: a 24-hour time, such as `07:30` or `18:00`.
- `--days`: three-letter day names, in a list, a range or both, such as `mon-fri`, `mon,wed,fri` or `mon-wed,sat`. A range that passes Sunday wraps round, so `fri-mon` is a long weekend.
- `--zone`: an IANA time zone, such as `America/Chicago` or `Europe/Berlin`, or `local` for the machine's own. If the machine's own isn't known, `local` is Central, `America/Chicago`, and the CLI says so.

```sh
silkpool auto-resume on --at 6:45 --days mon,wed,fri --zone Europe/Berlin
silkpool auto-resume set --zone local
silkpool auto-resume off
```

In the tray and on the dashboard, the **Auto-resume** switch turns it on or off at once, and **Edit** opens its editor, with the switch, the time, the days and the time zone. **Save** applies it, and **Cancel** leaves it as it was. Below the switch, auto-resume's summary says what pausing now would do, as the CLI's second line does.

While the node is Paused, a change moves the pause's end to the first resume time after now. Turning auto-resume off while the node is Paused makes it Paused indefinitely, from the tray, the CLI or the dashboard, and the CLI says "build-pc is now paused until you resume it."

The node's agent keeps auto-resume, not the tray app or the Hub, so it works while the tray app is closed, while nobody is signed in, and while the Hub can't reach the node. Quitting the tray app, with **Quit Silkpool's tray** in its icon's right-click menu, changes nothing on the node, and it starts again when an administrator next signs in. If the machine is off or asleep at the resume time, the node resumes as soon as its agent runs again.

## The slot limit

The **slot limit** is how many slots a node has, each running one job at a time: from 1 to 64, and 1 for a new node.

```sh
silkpool slot-limit
silkpool slot-limit 3
```

```text
Slot limit 3. Getting a runner ready for slot 3.
```

In the tray and on the dashboard, the **−** and **+** beside **Slots** lower and raise it.

- **Raising it** gets runners ready for the new slots at once, while the node is on duty.
- **Lowering it** never cancels a job. A slot above the new limit closes once its job ends: `Slot limit 1. Slot 2 closes after build-windows.`
- **While the node is paused,** the change waits: `Slot limit 3. The new slot limit takes effect when you resume.`

Each slot uses the machine's memory and CPU. On Windows and Linux, a **budget** also bounds what the slots may use together, with `silkpool budget`: the install pages for [Windows](install-node-windows.md#the-machines-resources) and [Linux](install-node-linux.md#the-machines-resources) give it. A Mac node has no budget, so [its slot limit is the only lever](install-node-macos.md#the-machines-resources).

## Emergency stop

**Emergency stop** cancels the node's running jobs at once, and leaves it Paused indefinitely until you resume it. Use it when a job mustn't finish: pausing would let it run to the end. GitHub marks each cancelled job failed, so check them there, and re-run any you still need.

It asks first, naming the jobs it cancels:

- **In the tray and on the dashboard,** **Stop now…** shows in red while jobs are running. Its confirmation says, for one job, "Stop now cancels build-windows. GitHub marks it failed, and build-pc stays paused until you resume it." **Stop and cancel** goes ahead, and **Keep running** leaves the jobs alone.
- **With the CLI:**

  ```sh
  silkpool emergency-stop
  ```

  ```text
  Stop now cancels build-windows. GitHub marks it failed, and build-pc stays paused until you resume it.
  Stop and cancel on build-pc? [y/N] y
  Stopped now, cancelling build-windows. build-pc stays paused until you resume it.
  ```

  Anything but `y` or `yes`, in either case, changes nothing. Where there's no terminal to ask on, as in a script, it refuses unless you give `--yes`: `silkpool emergency-stop --yes`. The CLI also works with no job running, which only leaves the node Paused indefinitely.

While a node is being removed, it refuses pausing, resuming, and changes of auto-resume, the slot limit and the budget, but accepts Emergency stop, and the dashboard says "Being removed. Only Emergency stop is accepted."

## Notices

A **notice** shows beside a node's state about something you may need to act on, such as a job that was interrupted. It never changes the state. One about something that happened stays until you dismiss it: with **Dismiss** in the tray or on the dashboard, or with the CLI, which dismisses all of them at once:

```sh
silkpool dismiss
```

One about a condition can't be dismissed, and clears by itself once the condition ends. `silkpool dismiss` lists each one still showing, with what clears it.

## Every command

These are all the `silkpool` commands, as `silkpool help` gives them:

| | |
|---|---|
| `silkpool status [--json] [--watch]` | Show what this node is doing. |
| `silkpool dismiss [--json]` | Dismiss the notices about something that happened. |
| `silkpool pause [--indefinitely] [--json]` | Stop taking new jobs. Running jobs finish first. |
| `silkpool resume [--revision N] [--json]` | Take jobs again. |
| `silkpool emergency-stop [--yes] [--json]` | Cancel the running jobs and stay paused. |
| `silkpool auto-resume [on\|set\|off] [flags]` | Show or change when a pause ends by itself. |
| `silkpool slot-limit [<n>] [--revision N]` | Show or change how many slots this node has. |
| `silkpool budget [--memory\|--cpus\|--none]` | Show or change the memory and CPU its slots may use. |
| `silkpool enroll <code> [--name <name>]` | Enroll this machine in a Hub. Needs sudo. |
| `silkpool remove [--force] [--yes] [--json]` | Remove this machine from its Hub. Needs sudo. |
| `silkpool set-hub-url <url> [--json]` | Point this node at its Hub's new URL. Needs sudo. |
| `silkpool diagnostics [--json]` | Print the diagnostics report. |
| `silkpool version [--json]` | Print the CLI's and the service's versions. |

On Windows, "Needs sudo" means a PowerShell run as administrator. The install pages give `enroll`, `remove`, `budget` and `diagnostics`, and [Install a Hub on Linux](install-hub-linux.md) gives `set-hub-url`. `silkpool version` prints the CLI's version and the running service's, which differ only until an upgrade has restarted the service:

```text
CLI      <version>
Service  <version>
```

Windows and macOS also have `silkpool uninstall`, which the install pages give, and `silkpool help` leaves out. On Windows, the Apps entry runs it, and `install.ps1` runs `silkpool install`.

### In scripts

Every command also prints JSON with `--json`, for scripts to read. A command that asks first, run with a terminal, prints its question too, unless you give `--yes`. A script that reads the node's settings, then acts on what it read, passes the revision it read back with `--revision`, as `silkpool resume --revision 7`, so that the command doesn't apply if the node has changed since. `resume`, `auto-resume on` and `set`, `slot-limit` and `budget` take it, and it guards what [Resume](#resume) says is checked: a lowered slot limit or a tightened budget applies whatever the revision. The exit codes are:

| | |
|---|---|
| 0 | applied, or nothing to change |
| 1 | refused |
| 2 | a usage error, such as a command that asks with no terminal to ask on |
| 3 | it couldn't ask the agent, such as when its service isn't running |
| 4 | Not confirmed: the connection to Silkpool's service dropped before it answered, twice, so it may or may not have applied. Run `silkpool status` to see whether it applied. |
