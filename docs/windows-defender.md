# Windows nodes: Microsoft Defender's exclusions

Microsoft Defender Antivirus can flag `silkpool.exe` as malware and quarantine it, which breaks the node. Silkpool isn't malware: Defender's machine-learning detection has mistaken it for some. So installing a Windows node includes a step: before the install, tell Defender not to scan Silkpool's two folders. **That weakens Defender's protection of the machine.** Read [What the exclusions cost](#what-the-exclusions-cost) before you add them.

## The symptom

Defender detects **`Trojan:Win32/Bearfoos.A!ml`** in `silkpool.exe`. It was seen on Windows 11 with real-time protection on, and it can come minutes after the install has succeeded:

- `silkpool` commands fail with "Operation did not complete successfully because the file contains a virus or potentially unwanted software".
- Defender removes `C:\Program Files\Silkpool\silkpool.exe`. The agent's service may keep running from memory until it stops, then it can't start again.
- Windows Security's **Protection history** lists the detection. Its affected items can include Silkpool's service, its process and its Apps entry in the registry, not only the program.

Silkpool's Windows programs aren't signed, and there are no plans to sign them. Defender sometimes flags unsigned programs that create accounts and services, as `silkpool install` does. The exclusions are the supported way to run Silkpool where it does.

## What the exclusions cost

Defender's antivirus doesn't scan files in an excluded folder, or in the folders below it: not when they're written, opened or run, and not in a scan. Its other protections, such as behaviour monitoring, can still raise an alert. With these exclusions:

- `C:\Program Files\Silkpool` holds Silkpool's programs. Only administrators can write there.
- `C:\ProgramData\Silkpool` holds the node's state and logs, and each slot's runner. **That's where your jobs run.** Each runner's `_work` folder, with the repositories a job checks out, what it builds and downloads, and the tools it sets up, is in this folder, and so is each slot's temporary folder. So Defender's antivirus doesn't scan what a job checks out, downloads, builds or runs there.

If a job brings something malicious into those folders, whether a dependency, a tool or a build's input, Defender's antivirus won't catch it there. A job can write elsewhere too, and Defender still scans that.

Add the exclusions only on a machine where you accept that trade-off. Without them, Defender may quarantine Silkpool at any time, which breaks the node. If your organisation manages Defender with Group Policy, Intune or another tool, ask whoever manages it: a managed machine can ignore exclusions set on it locally, and an exclusion may need their approval.

## Before you install: add the exclusions

In a PowerShell run as administrator, before you run the install command:

```powershell
Add-MpPreference -ExclusionPath 'C:\Program Files\Silkpool', 'C:\ProgramData\Silkpool'
```

The folders don't have to exist yet. To check them:

```powershell
(Get-MpPreference).ExclusionPath
```

Both folders should be listed. Then install as [Install a node on Windows](install-node-windows.md#install-and-enroll-it) says.

### If Defender flags the install itself

`install.ps1` downloads the release's zip into a temporary folder, unzips it there, and runs `silkpool.exe install` from that folder, which copies the programs into `C:\Program Files\Silkpool`. The exclusions above don't cover that temporary copy. If Defender flags it, the install stops with the virus message and Silkpool isn't installed.

Don't exclude your temporary folder: other programs use it too. Install from a folder of your own instead, excluded for the install alone. In a PowerShell run as administrator, with `<version>` the release's version and `amd64` or `arm64` for the machine:

```powershell
$dir = 'C:\SilkpoolSetup'
New-Item -ItemType Directory -Path $dir -ErrorAction Stop
```

If `New-Item` fails because the folder already exists, stop: pick a path that isn't in use for `$dir`, and run it again. The steps below delete `$dir`, so it must be a folder made here. Then:

```powershell
Add-MpPreference -ExclusionPath $dir
$release = 'https://github.com/meschoening/silkpool/releases/download/v<version>'
$zip = 'silkpool_<version>_windows_amd64.zip'
Invoke-WebRequest -UseBasicParsing -Uri "$release/$zip" -OutFile "$dir\$zip"
Invoke-WebRequest -UseBasicParsing -Uri "$release/SHA256SUMS" -OutFile "$dir\SHA256SUMS"
(Get-FileHash -Algorithm SHA256 "$dir\$zip").Hash
Select-String -SimpleMatch $zip "$dir\SHA256SUMS"
```

The two SHA-256s must match. If they don't, stop, and delete the folder and its exclusion as below: the zip isn't the release's. If they do:

```powershell
Expand-Archive -LiteralPath "$dir\$zip" -DestinationPath "$dir\silkpool"
& "$dir\silkpool\silkpool.exe" install
```

To enroll the machine, give the installed program's full path, since this PowerShell's `PATH` predates the install. Replace `<code>` with the enrollment code from the Hub's Add node page:

```powershell
& 'C:\Program Files\Silkpool\silkpool.exe' enroll '<code>'
```

The agent's service takes a few seconds to start after the install. If `enroll` says Silkpool's service isn't running, wait a moment and run it again.

Whether the install worked or not, delete the folder and its exclusion when you're done:

```powershell
Remove-Item -Recurse -Force $dir
Remove-MpPreference -ExclusionPath $dir
```

## If Defender has already quarantined it

Add the exclusions first, as above. Otherwise Defender flags `silkpool.exe` again as soon as it's back.

The surest fix is to **run the same install command again**, from the same release, without an enrollment code. Installing a release over itself repairs it: it puts the programs back and registers the service again. The node stays enrolled, because its state in `C:\ProgramData\Silkpool` is kept.

Or restore what Defender quarantined:

- **In Windows Security:** Virus & threat protection > Protection history. Open the `Trojan:Win32/Bearfoos.A!ml` detection and check that its affected items are Silkpool's, such as `C:\Program Files\Silkpool\silkpool.exe`. Only then, under Actions, choose Restore.
- **From the command line**, in a PowerShell run as administrator:

  ```powershell
  & "$env:ProgramFiles\Windows Defender\MpCmdRun.exe" -Restore -ListAll
  & "$env:ProgramFiles\Windows Defender\MpCmdRun.exe" -Restore -FilePath 'C:\Program Files\Silkpool\silkpool.exe'
  ```

  The first lists what's quarantined. The second restores `silkpool.exe` alone, to where it was. Don't restore by the detection's name with `-Name` and `-All`: that restores every file quarantined under it, which may include something else. There's no PowerShell cmdlet that restores a quarantined item ([MpCmdRun](https://learn.microsoft.com/en-us/defender-endpoint/command-line-arguments-microsoft-defender-antivirus)).

A restore puts back only what Defender removed. If the node doesn't come back, reinstall as above. Either way, check the node with `silkpool status`.

## Removing the exclusions

Remove them once you've uninstalled Silkpool. In a PowerShell run as administrator:

```powershell
Remove-MpPreference -ExclusionPath 'C:\Program Files\Silkpool', 'C:\ProgramData\Silkpool'
```

If Silkpool is still installed, Defender may quarantine it again once they're gone.
