# macOS nodes: Full Disk Access

A Mac node needs **Full Disk Access** for Silkpool, given in System Settings. Without it, the node stops taking jobs, and it shows Degraded, "Silkpool needs Full Disk Access", until you give it. Read [What it costs](#what-it-costs) first: Silkpool's jobs get it too.

## The symptom

On the Hub's dashboard, in the tray app and in `silkpool status`, the node is Degraded:

```
Degraded · Silkpool needs Full Disk Access.
```

Its slots have no runner, and it asks the Hub for none. Nothing else on the Mac changes.

## Why it happens

Every slot runs its jobs as one dedicated account, Silkpool's service account. Between jobs, once every slot is idle, Silkpool cleans that account, so one job can't leave anything for the next. Part of the clean is emptying the account's temporary folders, which macOS keeps under `/var/folders`.

macOS's privacy protection refuses some of what's in those folders to any program without Full Disk Access, even one running as root. A job that builds, signs or opens apps wakes macOS's certificate service, `trustd`, for the service account, and `trustd` makes a folder there that macOS won't let Silkpool list without Full Disk Access. If Silkpool can't empty the folders, it can't show the account is clean, so it doesn't start another job.

So before each runner, Silkpool checks that it has Full Disk Access, and shows the reason while it hasn't.

## What to set

On the Mac, as an administrator, once Silkpool is installed ([Install a node on macOS](install-node-macos.md)):

1. Open **System Settings ▸ Privacy & Security ▸ Full Disk Access**.
2. Click **+** at the bottom of the list, and enter your password if macOS asks for it.
3. In the window that opens, press **⌘⇧G**, enter `/usr/local/bin/silkpool`, and press Return.
4. Click **Open**.
5. Make sure **silkpool** is turned on in the list.

Silkpool picks it up within seconds. You don't need to restart anything: the next time a slot is prepared, which may take up to 5 minutes after a run of failures, the reason goes and the slot gets a runner.

The list may already show **silkpool**, turned off: macOS adds a program it has refused. Turning that entry on works too, if it's the one for `/usr/local/bin/silkpool`. The list doesn't show paths, so adding it as above is the sure way.

**On a Mac your organisation manages** with an MDM, ask whoever manages it. A managed Mac can have Full Disk Access given by a configuration profile, for `/usr/local/bin/silkpool` and its code signature, and may not let you change it in System Settings.

Giving Full Disk Access needs someone at the Mac, or screen sharing: macOS doesn't let a command over SSH give it.

## What it costs

Full Disk Access goes to the program `/usr/local/bin/silkpool`. That's Silkpool's agent, and it's also the program every slot's job starts from, as the service account. macOS treats everything a slot runs, your jobs' steps included, as that program. So **with Full Disk Access, your jobs have it too.**

- **What a job gains:** it can open what macOS's privacy protection keeps from other programs, wherever the file's own permissions let the service account in. On a Mac as it comes, that includes the database of which apps you've given privacy permissions, `/Library/Application Support/com.apple.TCC/TCC.db`, and Time Machine's settings, which every account may read but macOS otherwise keeps from programs. It doesn't get past file permissions: your own home's folders, such as Desktop, Documents, Mail and Safari's data, stay closed to the service account as they are now.
- **Any account on the Mac can borrow it,** within its own file permissions, by having launchd start `/usr/local/bin/silkpool` for it, as a job can.
- **What Silkpool still does:** before its slots get runners, Silkpool checks which places outside the slots the service account can open, through the same program, so the check sees what a job with Full Disk Access sees. If the account can open something it shouldn't, the node shows Degraded, "Slot can see files outside its own", as without Full Disk Access. A job with Full Disk Access can schedule work with `cron` or `at`, which macOS refuses a job without it. The clean between jobs removes that too, and if you take Full Disk Access away, the clean waits until you give it back, since it can't check what's scheduled.

Give it only on a Mac where you accept that, as you accept that the slots' jobs run there at all.

## How to check

In System Settings ▸ Privacy & Security ▸ Full Disk Access, **silkpool** is in the list and turned on. On the node, the reason is gone: `silkpool status` no longer reads "Silkpool needs Full Disk Access". If it comes back, after an upgrade of Silkpool or of macOS, say, check the list again.

## Removing it

`sudo silkpool uninstall` leaves Full Disk Access as it is. To take it away, select **silkpool** in the list and click **–**. If the node is still installed, it shows the reason again at its next slot.
