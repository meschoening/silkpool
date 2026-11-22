# Installs Silkpool on Windows from the release this copy was published
# with (ADR 0013). Run it from a PowerShell run as administrator:
#
#   & ([scriptblock]::Create((irm https://github.com/meschoening/silkpool/releases/download/v<version>/install.ps1)))
#   & ([scriptblock]::Create((irm .../install.ps1))) -Code <enrollment code>
#
# It downloads the release's zip for this machine, checks it against the
# release's SHA256SUMS, extracts it to a temporary folder, and runs
# silkpool.exe install from there, which installs Silkpool into Program
# Files and starts its service. Given an enrollment code, it then waits
# for the service and enrolls the machine. An upgrade is the same command
# for the newer release, without a code.
#
# It's written for Windows PowerShell 5.1, which every Windows has, and
# runs in PowerShell 7 too. It's plain ASCII, since Windows PowerShell
# reads a script without a byte order mark as the machine's code page.
#
# Everything runs from main, on the last lines, so a download cut short
# runs nothing.
param([string]$Code)

# The release's build fills these in.
$version = '@VERSION@'
$zips = @{ amd64 = '@ZIP_AMD64@'; arm64 = '@ZIP_ARM64@' }

$release = "https://github.com/meschoening/silkpool/releases/download/v$version"
# Where silkpool install puts the programs, and the agent's control pipe,
# which its service makes as it starts. ProgramW6432 is the real Program
# Files, even in a 32-bit PowerShell.
$programFiles = if ($env:ProgramW6432) { $env:ProgramW6432 } else { $env:ProgramFiles }
$installDir = 'Silkpool'
$program = 'silkpool.exe'
$pipe = 'silkpool'
# How long, in seconds, it waits for the service before it enrolls.
$serviceWait = 30

# fail stops main, which run reports as install.ps1's error, exiting
# with exitCode.
function fail([string]$message, [int]$exitCode = 1) {
	$e = [System.Exception]::new($message)
	$e.Data['install.ps1 exit code'] = $exitCode
	throw $e
}

# Test-Administrator reports whether this PowerShell runs elevated.
function Test-Administrator {
	$id = [Security.Principal.WindowsIdentity]::GetCurrent()
	([Security.Principal.WindowsPrincipal]$id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

# Get-MachineArch returns the machine's processor architecture, which a
# 32-bit PowerShell on a 64-bit Windows finds in PROCESSOR_ARCHITEW6432.
function Get-MachineArch {
	if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
}

# Get-ZipArch returns the architecture of the zip for a processor
# architecture, or nothing if there's none.
function Get-ZipArch([string]$machine) {
	switch ($machine) {
		'AMD64' { 'amd64' }
		'ARM64' { 'arm64' }
	}
}

# Enable-Tls12 lets Windows PowerShell 5.1 use TLS 1.2, which GitHub
# requires, where .NET leaves it out. The system's default already has
# it.
function Enable-Tls12 {
	$protocols = [Net.ServicePointManager]::SecurityProtocol
	if ([int]$protocols -ne 0 -and -not ($protocols -band [Net.SecurityProtocolType]::Tls12)) {
		[Net.ServicePointManager]::SecurityProtocol = $protocols -bor [Net.SecurityProtocolType]::Tls12
	}
}

# Save-ReleaseFile downloads a file of the release into the folder dir.
function Save-ReleaseFile([string]$name, [string]$dir) {
	$url = "$release/$name"
	try {
		Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile (Join-Path $dir $name) -ErrorAction Stop
	} catch {
		# The innermost error says why, such as a certificate that isn't
		# trusted, where the outer one says only that the connection closed.
		fail "couldn't download ${url}: $($_.Exception.GetBaseException().Message)"
	}
}

# Get-ListedSums returns the SHA-256s that the release's SHA256SUMS, in
# the folder dir, lists for a file, as sha256sum writes them, in text or
# binary mode.
function Get-ListedSums([string]$name, [string]$dir) {
	foreach ($line in [IO.File]::ReadAllLines((Join-Path $dir 'SHA256SUMS'))) {
		$fields = @($line.Trim() -split '\s+')
		if ($fields.Count -eq 2 -and ($fields[1] -ceq $name -or $fields[1] -ceq "*$name")) {
			$fields[0]
		}
	}
}

# Assert-Sum fails unless a downloaded file in the folder dir matches
# its one line in the release's SHA256SUMS.
function Assert-Sum([string]$name, [string]$dir) {
	$want = @(Get-ListedSums $name $dir)
	if ($want.Count -eq 0) {
		fail "the release's SHA256SUMS has no line for $name, so it wasn't installed"
	}
	if ($want.Count -gt 1) {
		fail "the release's SHA256SUMS has more than one line for $name, so it wasn't installed"
	}
	$got = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $dir $name) -ErrorAction Stop).Hash
	if ($got -ne $want[0]) {
		fail "$name doesn't match its SHA-256 in the release's SHA256SUMS, so it wasn't installed"
	}
}

# Test-Pipe reports whether the agent's pipe is there, by listing the
# machine's pipes, which, unlike opening it, leaves the agent alone.
function Test-Pipe {
	$path = "\\.\pipe\$pipe"
	try {
		$pipes = [IO.Directory]::GetFiles('\\.\pipe\')
	} catch {
		# An older .NET refuses some pipes' names in a listing.
		return Test-Path -LiteralPath $path
	}
	foreach ($p in $pipes) {
		if ($p -eq $path) { return $true }
	}
	$false
}

# Wait-Service waits up to serviceWait seconds for the agent's service
# to make its pipe: silkpool install returns once the service manager
# has started it, before the agent listens.
function Wait-Service {
	$deadline = [DateTime]::UtcNow.AddSeconds($serviceWait)
	while (-not (Test-Pipe)) {
		if ([DateTime]::UtcNow -ge $deadline) { return $false }
		Start-Sleep -Milliseconds 250
	}
	$true
}

# Remove-Folder deletes a temporary folder, trying again for a few
# seconds while something, such as an antivirus scanning what it
# extracted, holds a file in it.
function Remove-Folder([string]$dir) {
	for ($i = 0; ; $i++) {
		try {
			Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction Stop
			return
		} catch {
			if (-not (Test-Path -LiteralPath $dir)) { return }
			if ($i -ge 20) {
				[Console]::Error.WriteLine("install.ps1: couldn't delete ${dir}: $($_.Exception.Message)")
				return
			}
			Start-Sleep -Milliseconds 250
		}
	}
}

function main([string]$Code) {
	Set-StrictMode -Version 2.0
	# Windows PowerShell 5.1's progress bar slows downloads to a crawl.
	$ProgressPreference = 'SilentlyContinue'
	if ($version -like '@*') {
		fail "this copy isn't from a release: download install.ps1 from one, at https://github.com/meschoening/silkpool/releases"
	}
	if (-not (Test-Administrator)) {
		fail "it installs Silkpool's service, so run it from a PowerShell run as administrator"
	}
	$machine = Get-MachineArch
	$arch = Get-ZipArch $machine
	if (-not $arch) {
		fail "Silkpool's Windows package is for x64 and ARM64 machines, not $machine"
	}
	$zip = $zips[$arch]
	Enable-Tls12

	$tmp = Join-Path ([IO.Path]::GetTempPath()) ('silkpool-' + [guid]::NewGuid().ToString('N'))
	$null = New-Item -ItemType Directory -Path $tmp -ErrorAction Stop
	try {
		Save-ReleaseFile 'SHA256SUMS' $tmp
		if (@(Get-ListedSums $zip $tmp).Count -eq 0) {
			fail "this release has no Windows package"
		}
		Write-Host "Downloading $zip from $release"
		Save-ReleaseFile $zip $tmp
		Assert-Sum $zip $tmp
		$extracted = Join-Path $tmp 'silkpool'
		Expand-Archive -LiteralPath (Join-Path $tmp $zip) -DestinationPath $extracted -ErrorAction Stop
		$installer = Join-Path $extracted $program
		if (-not (Test-Path -LiteralPath $installer -PathType Leaf)) {
			fail "$zip has no $program, so nothing was installed"
		}
		Write-Host "Installing $zip with silkpool install"
		# Piped to the host, so that what it prints is shown, not
		# returned.
		& $installer install | Out-Host
		if ($LASTEXITCODE -ne 0) {
			fail "silkpool install failed, exiting $LASTEXITCODE" $LASTEXITCODE
		}
	} finally {
		Remove-Folder $tmp
	}

	if ($Code) {
		if (-not (Wait-Service)) {
			fail "Silkpool's service didn't start within $serviceWait s, so this machine isn't enrolled. Once it has, enroll it from a PowerShell run as administrator with: silkpool enroll <code>"
		}
		$installed = Join-Path (Join-Path $programFiles $installDir) $program
		if (-not (Test-Path -LiteralPath $installed -PathType Leaf)) {
			fail "silkpool install put no $installed there, so this machine isn't enrolled"
		}
		& $installed enroll $Code | Out-Host
		if ($LASTEXITCODE -ne 0) {
			fail "silkpool enroll failed, exiting $LASTEXITCODE" $LASTEXITCODE
		}
	}
}

# run runs main, and returns its exit code, having written why it
# failed, if it did, to standard error.
function run([string]$Code) {
	try {
		$null = main $Code
		0
	} catch {
		$e = $_.Exception
		[Console]::Error.WriteLine("install.ps1: $($e.Message)")
		if ($e.Data.Contains('install.ps1 exit code')) { $e.Data['install.ps1 exit code'] } else { 1 }
	}
}

# install.ps1's tests dot-source it with SILKPOOL_INSTALL_TEST set, and
# call run themselves.
if (-not $env:SILKPOOL_INSTALL_TEST) {
	$status = run $Code
	if ($status -ne 0) {
		# Run as a file, it exits with the code. Run from irm, as a script
		# block in someone's PowerShell, exiting would close their window,
		# so it stops with an error instead, which a PowerShell given it
		# as a command exits 1 for.
		if ($PSCommandPath) { exit $status }
		$global:LASTEXITCODE = $status
		throw "install.ps1 didn't finish, exiting $status"
	}
}
