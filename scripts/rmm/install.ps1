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
    The customer folder to protect. Must already exist.

.PARAMETER Token
    One-time enrollment token. Defaults to $env:SOFTAFRIQUE_ENROLL_TOKEN.

.EXAMPLE
    $env:SOFTAFRIQUE_ENROLL_TOKEN = $rmmVar
    .\install.ps1 -MsiPath C:\Temp\agent.msi -BackupPath "D:\CustomerData"
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$MsiPath,
    [Parameter(Mandatory = $true)][string]$BackupPath,
    [string]$Token = $env:SOFTAFRIQUE_ENROLL_TOKEN,
    [int]$TimeoutSeconds = 300
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
if (-not (Test-Path $BackupPath -PathType Container)) {
    Write-Fail "backup folder $BackupPath does not exist on this machine"
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

$proc = Start-Process -FilePath 'msiexec.exe' -ArgumentList $msiArgs -Wait -PassThru
if ($proc.ExitCode -ne 0) {
    $tail = (Get-Content $log -Tail 25 -ErrorAction SilentlyContinue) -join "`n"
    Write-Fail "msiexec exited $($proc.ExitCode). Last lines of $log :`n$tail"
}

if (-not (Test-Path $AgentExe)) { Write-Fail "the agent was not installed at $AgentExe" }
Write-Step 'agent installed'

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
