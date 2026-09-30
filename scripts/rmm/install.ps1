<#
.SYNOPSIS
    Installs the Softafrique Backup Agent and enrolls it with the gateway.

.DESCRIPTION
    Run this from Tactical RMM. It does two things that must both happen, and it
    reports which one failed:

      1. Installs the MSI silently, with the folder to protect baked into
         config.yaml and the data directory ACL'd to SYSTEM and Administrators.
      2. Enrolls the device, which is what exchanges the one-time token for the
         device id, the repository and the encryption key.

    The token is read from the environment (SOFTAFRIQUE_ENROLL_TOKEN), never from
    a command line. A command line is visible to every process on the machine
    through the process table, and RMM writes script command lines into its own
    logs.

.PARAMETER MsiPath
    Full path to SoftafriqueBackupAgent.msi. Usually supplied by an RMM file
    download step into $env:ProgramFiles\Tactical RMM or a temp directory.

.PARAMETER BackupPath
    The customer folder to protect. Created if it is missing. Must be on a fixed
    disk; a network share is refused unless -AllowUNC is given.

.PARAMETER AllowUNC
    Permit a network share as the folder to protect. Off by default, and there
    is a reason. The service runs as LocalSystem and reaches a share as the
    machine account, so a path that opened fine for the technician typing it can
    fail every hourly backup because the machine account was never granted
    access, silently. Use this only for a share whose ACL already grants the
    machine account read, and test it afterwards.

.PARAMETER Token
    One-time enrollment token. Defaults to $env:SOFTAFRIQUE_ENROLL_TOKEN.

.EXAMPLE
    $env:SOFTAFRIQUE_ENROLL_TOKEN = $rmmVar
    .\install.ps1 -MsiPath C:\Temp\agent.msi -BackupPath "D:\CustomerData"

.EXAMPLE
    $env:SOFTAFRIQUE_ENROLL_TOKEN = $rmmVar
    .\install.ps1 -MsiPath C:\Temp\agent.msi -BackupPath "\\FILESERVER\CustomerData" -AllowUNC
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$MsiPath,
    [Parameter(Mandatory = $true)][string]$BackupPath,
    [string]$Token = $env:SOFTAFRIQUE_ENROLL_TOKEN,
    [int]$TimeoutSeconds = 300,
    [switch]$AllowUNC
)

$ErrorActionPreference = 'Stop'
$ServiceName = 'SoftafriqueBackupAgent'
$DataDir = Join-Path $env:ProgramData 'SoftafriqueBackupAgent'
$AgentExe = Join-Path $env:ProgramFiles 'Softafrique Backup Agent\SoftafriqueBackupAgent.exe'

function Write-Step([string]$Message) {
    Write-Output "[install] $Message"
}

function Write-Fail([string]$Message) {
    Write-Output "[install] FAILED: $Message"
    # A non-zero exit is what RMM shows as a failed task; the message is what the
    # technician reads, so it has to say what to do next.
    exit 1
}

if (-not (Test-Path $MsiPath)) { Write-Fail "MSI not found at $MsiPath" }
if ([string]::IsNullOrWhiteSpace($Token)) {
    Write-Fail 'no enrollment token: set SOFTAFRIQUE_ENROLL_TOKEN in the RMM script variable'
}

# The backup folder must be on a fixed disk, checked before anything is created
# so nothing is made on a drive we are about to refuse.
#
# This exists because of a Server 2019 test machine where the backup path was
# pointed at D:, which was the DVD drive. Every check the installer could
# express passed, so the install succeeded and then every scheduled backup failed
# with "device is not ready". The MSI's own validatepath.exe enforces this too,
# but it runs deferred as SYSTEM, so without a check here the technician gets an
# msiexec exit code and a log tail instead of a sentence saying what is wrong.
$volumeOf = if ($BackupPath -like '\\*') { 'a network share' } else {
    $root = [System.IO.Path]::GetPathRoot($BackupPath)
    $drive = ($root -replace '[:\\]', '')
    if ([string]::IsNullOrEmpty($drive)) {
        Write-Fail "cannot work out which drive $BackupPath is on; give a full path like D:\CustomerData"
    }
    try {
        # ToLowerInvariant, not ToLower: on a Turkish-locale Windows "Fixed"
        # lowercases to "fıxed" with a dotless i, which would fail the comparison
        # below and refuse every install on that machine.
        ([System.IO.DriveInfo]::new("$drive`:")).DriveType.ToString().ToLowerInvariant()
    } catch {
        Write-Fail "cannot inspect drive ${drive}: $($_.Exception.Message)"
    }
}
# A network share is the one kind that can be allowed, and only when asked for by
# name. It is refused by default because the service reaches it as the machine
# account, which a technician logged in interactively does not test.
if ($volumeOf -ne 'fixed' -and -not ($AllowUNC -and $volumeOf -eq 'a network share')) {
    $how = if ($volumeOf -eq 'a network share') {
        'Pass -AllowUNC if this share is genuinely required and its permissions already grant the machine account access.'
    } else {
        'An optical or removable drive can be ejected between backups, and the backup then fails every time.'
    }
    Write-Fail ("backup path {0} is on {1}, not a fixed disk. A backup folder must be on a fixed disk: {2}" -f
        $BackupPath, $volumeOf, $how)
}
if ($AllowUNC -and $volumeOf -eq 'a network share') {
    Write-Step "ALLOWING a network share at $BackupPath on request: verify the machine account can read it before you close this task"
}

# The backup folder is created if it is missing, not refused.
#
# This used to be a hard failure, which was the strictest of the three checks in
# the install path and disagreed with both of the others: the MSI's own
# validatepath helper deliberately allowed a missing folder, and the agent
# recreates one at run time. A customer folder that does not exist yet is a
# normal thing to be asked to protect, and refusing it meant a technician had to
# create the folder by hand before the RMM would deploy.
#
# A path that exists but is a *file* is still refused, because that is a
# misconfiguration rather than a folder awaiting data.
if (Test-Path $BackupPath -PathType Leaf) {
    Write-Fail "backup path $BackupPath is a file, not a folder"
} elseif (-not (Test-Path $BackupPath -PathType Container)) {
    try {
        New-Item -ItemType Directory -Path $BackupPath -Force -ErrorAction Stop | Out-Null
        Write-Step "created backup folder $BackupPath"
    } catch {
        Write-Fail "could not create backup folder ${BackupPath}: $($_.Exception.Message)"
    }
}

$log = Join-Path $env:TEMP "softafrique-install-$([guid]::NewGuid().ToString('N')).log"
Write-Step "installing from $MsiPath"
Write-Step "protecting $BackupPath"
Write-Step "msi log: $log"

# /qn because RMM has nobody at the keyboard. The property assignment is how
# BACKUPPATH reaches the config file the MSI writes.
$msiArgs = @(
    '/i', "`"$MsiPath`"",
    '/qn',
    '/norestart',
    '/l*v', "`"$log`"",
    "BACKUPPATH=`"$BackupPath`""
)

# The same opt-in the script just honoured, passed on so the MSI's own
# validatepath.exe agrees. Without it the install would fail here even though
# this script was told the share is wanted.
if ($AllowUNC) { $msiArgs += 'ALLOWUNC=1' }

$proc = Start-Process -FilePath 'msiexec.exe' -ArgumentList $msiArgs -Wait -PassThru
if ($proc.ExitCode -ne 0) {
    $tail = (Get-Content $log -Tail 25 -ErrorAction SilentlyContinue) -join "`n"
    Write-Fail "msiexec exited $($proc.ExitCode). Last lines of $log :`n$tail"
}

if (-not (Test-Path $AgentExe)) { Write-Fail "the agent was not installed at $AgentExe" }
Write-Step 'agent installed'

# Record the opt-in in the config file as well, because the MSI property is gone
# by now and the agent still has to know at three in the morning.
#
# The MSI's own check is told by ALLOWUNC on the command line, but the agent's
# run-time half of the same rule reads config.yaml, and an install that allowed
# a share and then left the key false would pass the installer and refuse to
# back up. Writing it here is the one place both halves are decided by the same
# switch.
if ($AllowUNC) {
    $configPath = Join-Path $DataDir 'config.yaml'
    if (-not (Test-Path $configPath)) {
        Write-Fail "cannot record the network-share opt-in: $configPath is missing"
    }
    try {
        $text = Get-Content $configPath -Raw -ErrorAction Stop
        # Match the key with or without a leading comment block already above
        # it; only the value is being changed, so the surrounding explanation
        # stays exactly as the installer wrote it.
        $updated = [regex]::Replace($text, '(?m)^(allow_unc:\s*)(?:true|false)\s*$', '${1}true')
        if ($updated -eq $text) {
            Write-Fail ("cannot record the network-share opt-in: no 'allow_unc:' key found in {0}. " +
                'Add it by hand with the value true, or the agent will refuse the share.') -f $configPath
        }
        Set-Content -Path $configPath -Value $updated -NoNewline -Encoding UTF8 -ErrorAction Stop
        Write-Step "recorded allow_unc: true in $configPath"
    } catch {
        Write-Fail "could not record the network-share opt-in in ${configPath}: $($_.Exception.Message)"
    }
}

# The service is installed but not started yet, so a scheduled run cannot race
# enrollment. Start it after the device has credentials.
$svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if (-not $svc) { Write-Fail "the $ServiceName service was not registered by the MSI" }
if ($svc.StartType -ne 'Automatic') { Write-Fail "the service start type is $($svc.StartType), expected Automatic" }
Write-Step 'service registered and set to start automatically'

# ---- enrollment -------------------------------------------------------------
# The token goes into this process's environment, and the agent reads it from
# there. It is never on a command line and never written to a file.
$env:SOFTAFRIQUE_ENROLL_TOKEN = $Token
try {
    Write-Step 'enrolling with the gateway'
    $out = & $AgentExe enroll -config (Join-Path $DataDir 'config.yaml') 2>&1
    $code = $LASTEXITCODE
} finally {
    # Do not leave the token in this process's environment for the next step.
    Remove-Item Env:\SOFTAFRIQUE_ENROLL_TOKEN -ErrorAction SilentlyContinue
    $Token = $null
}

$out | ForEach-Object { Write-Output "[install] $_" }
if ($code -ne 0) {
    Write-Fail "enrollment failed with exit code $code (3 = not enrolled, 4 = the gateway wants something changed). Output above."
}

# ---- start and verify -------------------------------------------------------
Start-Service -Name $ServiceName
$svc = Get-Service -Name $ServiceName
if ($svc.Status -ne 'Running') { Write-Fail "the service did not start (status $($svc.Status))" }
Write-Step 'service running'

# Give the agent one poll interval's worth of slack for the first status write,
# then insist on a status file that says the device is enrolled. A status file
# that is missing or unenrolled here is the difference between "deployed" and
# "deployed and protecting", and it is worth waiting a few seconds to tell them
# apart.
$statusFile = Join-Path $DataDir 'status.json'
$deadline = (Get-Date).AddSeconds(60)
$enrolled = $false
while ((Get-Date) -lt $deadline) {
    if (Test-Path $statusFile) {
        try {
            $st = Get-Content $statusFile -Raw | ConvertFrom-Json
            if ($st.enrolled) { $enrolled = $true; break }
        } catch { Start-Sleep -Seconds 2; continue }
    }
    Start-Sleep -Seconds 2
}
if (-not $enrolled) {
    Write-Fail "installed and enrolled, but $statusFile does not yet say enrolled=true. Check $DataDir\agent.log"
}

$st = Get-Content $statusFile -Raw | ConvertFrom-Json
Write-Step "device_id   $($st.device_id)"
Write-Step "tenant      $($st.tenant)"
Write-Step "repo_host   $($st.repo_host)"
Write-Step "paths       $($st.backup_paths -join ', ')"
Write-Step "gateway     $($st.server_status)"
Write-Step "done. The first backup runs within minutes; check '.\health-check.ps1' for status."

# Exit 0 only when the device is enrolled and the service is running. Anything
# else exits non-zero so RMM flags the task.
exit 0
