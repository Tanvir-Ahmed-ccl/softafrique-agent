<#
.SYNOPSIS
    Removes the Softafrique Backup Agent from this machine.

.DESCRIPTION
    Removes the service, the program files and the sealed credentials, and
    leaves config.yaml, status.json and the log in place for diagnostics.

    Deleting credentials.dat is the default because an uninstalled agent must
    not leave a usable repository key on a decommissioned or reimaged machine.
    The repository and every snapshot stay on the server; a restore now needs
    the escrowed key from the dashboard. Uninstalling does not suspend the
    device at the gateway, so suspend it separately if the machine is finished.

    The MSI performs the same cleanup on a standalone uninstall. This script
    repeats it only as a fallback for an interrupted or older package.

.PARAMETER PurgeCredentials
    Also delete the whole data directory, including config.yaml, status.json,
    the log and restic's cache. Without this switch those diagnostic files are
    kept. The encryption key is deleted either way.

.PARAMETER KeepStatus
    Keep status.json. Useful when capturing it for a ticket before removal.

.PARAMETER MsiProductCode
    Installed product code to hand to msiexec /x, overriding the registry
    lookup. The default is to look the code up on the machine itself, which is
    what happens everywhere unless the lookup fails: Product.wxs declares no
    ProductCode, so WiX generates a new one on every build and no value written
    into this script can stay correct.

.EXAMPLE
    .\uninstall.ps1
    .\uninstall.ps1 -PurgeCredentials
#>
[CmdletBinding()]
param(
    [switch]$PurgeCredentials,
    [switch]$KeepStatus,
    [string]$MsiProductCode
)

$ErrorActionPreference = 'Stop'
$ServiceName = 'SoftafriqueBackupAgent'
$DataDir = Join-Path $env:ProgramData 'SoftafriqueBackupAgent'

function Write-Step([string]$Message) { Write-Output "[uninstall] $Message" }

# Find the installed product code in the Uninstall keys, because it cannot be
# written down in this file.
#
# Product.wxs declares an UpgradeCode but no ProductCode, so WiX generates a
# fresh ProductCode on every build. The value this script used to carry was the
# UpgradeCode itself, verbatim: msiexec /x can never match it, the exit code
# came back 1605, 1605 was counted as success, and the uninstall exited 0 with
# the service and the program files still in place. A hardcoded code would be
# wrong by the next build even if it started out right.
#
# Win32_Product is deliberately not used: reading it runs a consistency check
# across every installed MSI on the machine, which is slow enough to time out
# inside an RMM job.
function Get-InstalledProductCode {
    param([string]$NameLike = 'Softafrique Backup Agent')
    foreach ($r in @(
        'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*',
        'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*')) {
        $hit = Get-ItemProperty $r -ErrorAction SilentlyContinue |
               Where-Object { $_.DisplayName -like "*$NameLike*" -and
                              $_.PSChildName -match '^\{[0-9A-Fa-f-]{36}\}$' } |
               Select-Object -First 1
        if ($hit) { return $hit.PSChildName }
    }
    return $null
}

# -MsiProductCode overrides the lookup for a machine where the registry cannot
# be read; it is not the default, for the reason above.
if (-not $MsiProductCode) {
    $MsiProductCode = Get-InstalledProductCode
}

if (-not $MsiProductCode) {
    # Either the MSI was never installed (a manual install), or the lookup
    # failed. Which one it is has to be decided before anything is stopped or
    # removed: if the agent is still present, walking away quietly with a zero
    # exit is the exact bug this lookup exists to fix, so it fails with the two
    # ways out instead.
    $svcPresent = $null -ne (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)
    $exePresent = Test-Path (Join-Path $env:ProgramFiles 'Softafrique Backup Agent\SoftafriqueBackupAgent.exe')
    if ($svcPresent -or $exePresent) {
        Write-Output "[uninstall] FAILED: the agent is still present (service: $svcPresent, program files: $exePresent), but no installed MSI entry matched 'Softafrique Backup Agent'. Pass -MsiProductCode '<guid>' if the registry lookup cannot see it (reg query HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall /s), or for a manual install run SoftafriqueBackupAgent.exe uninstall and delete the program files by hand."
        exit 1
    }
    Write-Step 'not installed: no MSI entry, no service, no program files'
}

# Stop first, so the MSI is not trying to remove files a running restic has open.
$svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($svc) {
    if ($svc.Status -ne 'Stopped') {
        Write-Step "stopping the $ServiceName service"
        Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
        $svc.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(60))
    }
    Write-Step 'service stopped'
} else {
    Write-Step 'service was not installed'
}

# Capture the status before it goes, for the ticket.
$statusFile = Join-Path $DataDir 'status.json'
if (Test-Path $statusFile) {
    try {
        $st = Get-Content $statusFile -Raw | ConvertFrom-Json
        Write-Step "last state: device=$($st.device_id) last_success=$($st.last_success) last_snapshot=$($st.last_snapshot_id)"
    } catch {
        Write-Step 'status.json could not be read; continuing'
    }
    if ($KeepStatus) {
        $copy = Join-Path $env:TEMP "softafrique-status-$(Get-Date -Format yyyyMMdd-HHmmss).json"
        Copy-Item $statusFile $copy
        Write-Step "kept a copy of status.json at $copy"
    }
}

if ($MsiProductCode) {
    Write-Step "removing MSI product $MsiProductCode"
    $log = Join-Path $env:TEMP "softafrique-uninstall.log"
    $proc = Start-Process -FilePath 'msiexec.exe' `
        -ArgumentList @('/x', $MsiProductCode, '/qn', '/norestart', '/l*v', "`"$log`"") `
        -Wait -PassThru
    # 1605 is "already uninstalled", which is a success for this script's purpose.
    if ($proc.ExitCode -ne 0 -and $proc.ExitCode -ne 1605) {
        $tail = (Get-Content $log -Tail 25 -ErrorAction SilentlyContinue) -join "`n"
        Write-Output "[uninstall] FAILED: msiexec exited $($proc.ExitCode). Last lines of ${log}:`n$tail"
        exit 1
    }
    Write-Step 'package removed'
} else {
    # Only reachable on the not-installed path decided above, which has already
    # said so once; this line keeps the msiexec step visible in the RMM log.
    Write-Step 'no MSI package registered; nothing for msiexec to remove'
}

# The MSI removes its own files. The data directory survives, but the sealed
# credentials must not: the MSI's own uninstall action already deletes them, and
# this is the fallback for an interrupted uninstall or an older package.
$credentials = Join-Path $DataDir 'credentials.dat'
if (Test-Path $credentials) {
    Remove-Item $credentials -Force -ErrorAction SilentlyContinue
    if (Test-Path $credentials) {
        Write-Output "[uninstall] FAILED: could not delete $credentials. Delete it by hand."
        exit 1
    }
    Write-Step 'sealed credentials deleted from this machine; a restore now needs the escrowed key from the dashboard'
} else {
    Write-Step 'no sealed credentials to delete'
}

if ($PurgeCredentials) {
    if (Test-Path $DataDir) {
        Write-Step "deleting $DataDir, including config, status and the restic cache"
        Remove-Item $DataDir -Recurse -Force -ErrorAction SilentlyContinue
        if (Test-Path $DataDir) {
            Write-Output "[uninstall] FAILED: could not delete $DataDir. Delete it by hand."
            exit 1
        }
        Write-Step 'data directory removed'
    } else {
        Write-Step 'no data directory to purge'
    }
} else {
    if (Test-Path $DataDir) {
        Write-Step "kept $DataDir without credentials, for diagnostics. Snapshots on the server are untouched either way."
        Write-Step 'the device is still enrolled at the gateway; suspend it from the dashboard if it is decommissioned.'
    }
}

exit 0
