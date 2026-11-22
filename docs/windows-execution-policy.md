# Windows nodes: PowerShell's execution policy

GitHub's runner application runs a workflow's PowerShell steps as script files. If the node's PowerShell **execution policy** doesn't allow scripts, every such step fails. On a Windows client, the policy out of the box is **Restricted**, which allows none. Silkpool doesn't change the policy, but it tells you when it blocks scripts: `silkpool enroll` says so after enrolling the node, and `silkpool diagnostics` shows each PowerShell's policy.

## The symptom

A job's PowerShell step fails at once, and its log says the step's script "cannot be loaded because running scripts is disabled on this system":

```
...\_temp\<id>.ps1 cannot be loaded because running scripts is disabled on this system.
```

That's the message under Restricted. Other policies refuse a script with other messages.

The node itself is fine. Silkpool sees an ordinary failed job: the runner ran it to its end and the slot was reset as usual. The node doesn't show as Degraded, since its other jobs still run, so neither the Hub's dashboard nor `silkpool status` mentions it. `silkpool diagnostics` does (see [How to check](#how-to-check)).

## Why it happens

For a `pwsh` or `powershell` step, the runner writes the step's `run:` text to a `.ps1` file in the job's `_temp` folder. It then runs `pwsh -command ". '<file>'"` or `powershell -command ". '<file>'"`, which runs that file as a script. `pwsh` is the default shell on Windows. On a self-hosted runner without PowerShell 7, steps use Windows PowerShell (`powershell`) instead ([GitHub: `shell`](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax)). So a step with no `shell:` at all is affected too.

The step runs as Silkpool's service account, `silkpool`, so the policy that counts is the one in effect for that account. PowerShell takes the first scope that sets one, in this order ([about_Execution_Policies](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_execution_policies?view=powershell-5.1)):

| Scope | Set by | Notes |
|---|---|---|
| `MachinePolicy` | Group Policy, for the computer | Overrides everything below. |
| `UserPolicy` | Group Policy, for the user | Overrides everything below. |
| `Process` | `-ExecutionPolicy` on the command line, for that process only | |
| `CurrentUser` | `Set-ExecutionPolicy -Scope CurrentUser` | Kept in the user's profile. |
| `LocalMachine` | `Set-ExecutionPolicy -Scope LocalMachine` | |

If no scope sets one, Windows PowerShell's effective policy is **Restricted** on a Windows client and **RemoteSigned** on Windows Server. PowerShell 7's is **Restricted** on both.

Windows PowerShell and PowerShell 7 keep separate settings. Windows PowerShell keeps `LocalMachine` in the registry. PowerShell 7 keeps it in `powershell.config.json` in its own installation folder ([about_PowerShell_Config](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_powershell_config?view=powershell-7.5)). Setting one doesn't set the other. PowerShell 7's packages for Windows come with its `LocalMachine` policy set to RemoteSigned, so on a node that has it, `pwsh` steps usually run while `powershell` steps don't.

## What to set

You can set it before or after you install Silkpool. Its Windows installer works under Restricted in Windows PowerShell, run as the Add node page gives it: as a command, not a script file.

### On the node: RemoteSigned for the machine

**RemoteSigned** is the least permissive policy that runs the runner's step scripts:

- **Restricted** runs no scripts.
- **AllSigned** runs only scripts signed by a trusted publisher, and the runner's step scripts aren't signed.
- **RemoteSigned** runs unsigned scripts written on the machine. It still refuses an unsigned script that Windows has marked as downloaded from the internet, until it's unblocked.

Set it for the whole machine. In a PowerShell run as administrator:

```powershell
Set-ExecutionPolicy -ExecutionPolicy RemoteSigned -Scope LocalMachine
```

That sets Windows PowerShell's policy. If PowerShell 7 is installed, set it again from a `pwsh` run as administrator. The change takes effect at once, for every new PowerShell process.

**Don't use `-Scope CurrentUser` for Silkpool's service account.** That scope is kept in the account's profile, and Silkpool deletes the profile whenever it cleans the account, which it does once all the node's slots are idle. Setting it for your own account doesn't help either: jobs don't run as you.

Silkpool's own Windows tests run their jobs with the per-workflow fix below, not with RemoteSigned. RemoteSigned works by Microsoft's definition of it, but it isn't tested.

### In the workflow instead: bypass for one step

If you'd rather leave the node's policy alone, a workflow can set the policy for a step's own PowerShell process with a custom `shell:`:

```yaml
- shell: powershell -NoProfile -ExecutionPolicy Bypass -Command ". '{0}'"
  run: Write-Output "runs whatever the node's policy"
```

For PowerShell 7, write `pwsh` in place of `powershell`. The runner still writes the step as a `.ps1` file and wraps it as it does for the built-in `powershell` or `pwsh` shell. Only `-NoProfile -ExecutionPolicy Bypass` is added, and it lasts only as long as that process. Silkpool's own Windows test jobs use this line. It has to go on every PowerShell step, in every workflow that runs on the node. It can also go once in `defaults.run.shell`.

### If Group Policy sets it

If `MachinePolicy` or `UserPolicy` sets a policy, it overrides every other scope, including `-ExecutionPolicy` on the command line. Then neither fix above has any effect. Change the Group Policy setting instead. It's **Turn on Script Execution**, set to **Allow local scripts and remote signed scripts**, which is RemoteSigned:

- For Windows PowerShell, it's under Administrative Templates > Windows Components > Windows PowerShell.
- For PowerShell 7, it's under Administrative Templates > PowerShell Core, which has a "Use Windows PowerShell Policy setting" option ([about_Group_Policy_Settings](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_group_policy_settings?view=powershell-7.5)).

For PowerShell 7, `MachinePolicy` can also come from its own `powershell.config.json`, in its installation folder, when Group Policy doesn't set it: a `PowerShellPolicies` entry whose `ScriptExecution` sets `EnableScripts` and `ExecutionPolicy` ([about_PowerShell_Config](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_powershell_config?view=powershell-7.5)). If Group Policy Editor shows nothing, look there.

Setting it to **Disabled** is the same as Restricted.

## How to check

On the node, in a PowerShell run as administrator:

```powershell
silkpool diagnostics
```

Its **PowerShell** part has a line for Windows PowerShell and one for PowerShell 7: the policy in effect, where it came from, and whether it blocks a job's steps. `silkpool diagnostics --json` has the same in its `powershell` field. Silkpool reads the `MachinePolicy` and `LocalMachine` scopes, and PowerShell's default when neither sets one. It doesn't read `UserPolicy`, Group Policy for the service account itself, so if your Group Policy sets the policy for users, check from a job as below. It finds PowerShell 7 by the first `pwsh.exe` on the machine's `Path`, which is the one a job's steps use unless the workflow changes its `PATH`.

To see every scope, in a PowerShell run as administrator, and in `pwsh` too if PowerShell 7 is installed:

```powershell
Get-ExecutionPolicy -List
```

Look at the `MachinePolicy` and `LocalMachine` rows. The `UserPolicy` and `CurrentUser` rows there are your own, not the service account's.

To see what a job sees, run a `shell: cmd` step that starts PowerShell with a command instead of a script file, which even Restricted allows:

```yaml
- shell: cmd
  run: powershell -NoProfile -Command "Get-ExecutionPolicy -List"
```

For PowerShell 7, write `pwsh` in place of `powershell`. Every row in the job's log is for Silkpool's service account. If no row above `LocalMachine` sets a policy, `LocalMachine` is the one in effect, and it should read `RemoteSigned`.
