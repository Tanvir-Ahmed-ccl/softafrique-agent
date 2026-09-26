<#
.SYNOPSIS
    Removes the Softafrique Backup Agent from this machine.

.DESCRIPTION
    Removes the service and the program files, and by default leaves the sealed
    credentials and the repository alone.

    That default is deliberate. Uninstalling an agent does not unsuspend a
    device at the gateway, does not delete a single snapshot, and does not
    release the encryption key. A technician replacing a machine should be able
    to uninstall, reimage, reinstall and enroll, and the customer's backup
    history is still on the server either way. Use -PurgeCredentials only when
    the device is genuinely finished with.

.PARAMETER PurgeCredentials
    Also delete credentials.dat, which deletes the encryption key from this
    machine. After that a restore from this device is impossible without the
    escrowed key from the dashboard, so this is not reversible from here.

.PARAMETER KeepStatus
    Keep status.json. Useful when capturing it for a ticket before removal.

.EXAMPLE
    .\uninstall.ps1
    .\uninstall.ps1 -PurgeCredentials
#>
[CmdletBinding()]
param(
    [switch]$PurgeCredentials,
    [switch]$KeepStatus,
    [string]$MsiProductCode = '{6C7A1F52-9E4B-4E2C-9C1F-2D8A5B7E0F34}'
)

$ErrorActionPreference = 'Stop'
$ServiceName = 'SoftafriqueBackupAgent'
$DataDir = Join-Path $env:ProgramData 'SoftafriqueBackupAgent'

function Write-Step([string]$Message) { Write-Output "[uninstall] $Message" }

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

# The MSI removes its own files, but the data directory and the sealed
# credentials are per-machine state it deliberately leaves behind.
if ($PurgeCredentials) {
    if (Test-Path $DataDir) {
        Write-Step "deleting $DataDir, including the sealed encryption key"
        Remove-Item $DataDir -Recurse -Force -ErrorAction SilentlyContinue
        if (Test-Path $DataDir) {
            Write-Output "[uninstall] FAILED: could not delete $DataDir. Delete it by hand."
            exit 1
        }
        Write-Step 'encryption key deleted from this machine. A restore now needs the escrowed key from the dashboard.'
    } else {
        Write-Step 'no data directory to purge'
    }
} else {
    if (Test-Path $DataDir) {
        Write-Step "kept $DataDir (credentials and status). Snapshots on the server are untouched either way."
        Write-Step 'the device is still enrolled at the gateway; suspend it from the dashboard if it is decommissioned.'
    }
}

exit 0
