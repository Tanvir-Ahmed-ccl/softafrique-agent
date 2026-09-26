<#
.SYNOPSIS
    Checks whether this machine is actually protected, and says why not if it is not.

.DESCRIPTION
    The point of this script is to be alertable. "The service is running" is not
    the same as "the customer's data is being backed up", and an RMM dashboard
    that shows the first will happily report a machine that has not backed up in
    a week as healthy.

    So it reads status.json and checks the things that mean a customer is
    protected:

      * the device is enrolled at all;
      * the service is running;
      * the gateway has not suspended the device;
      * a backup has succeeded within a threshold that scales with the schedule;
      * no run has failed repeatedly.

    status.json is the agent's own record, and it deliberately contains no
    secrets, so this script can be run by a monitoring agent with read access
    and nothing more.

.PARAMETER AgentDir
    The agent data directory. Defaults to the installed location.

.PARAMETER MaxHoursSinceSuccess
    Alert if the last successful backup is older than this. 0 means derive it
    from the agent's own schedule_interval (three intervals' grace, minimum 2
    hours), which is the right default: a machine on a 1-hour schedule and a
    machine on a 24-hour schedule should not share one threshold.

.PARAMETER MaxConsecutiveFailures
    Alert after this many failures in a row. 0 disables the check, because a
    single failure is usually a laptop that was closed.

.EXAMPLE
    .\health-check.ps1
    .\health-check.ps1 -MaxHoursSinceSuccess 26 -Verbose
#>
[CmdletBinding()]
param(
    [string]$AgentDir = (Join-Path $env:ProgramData 'SoftafriqueBackupAgent'),
    [double]$MaxHoursSinceSuccess = 0,
    [int]$MaxConsecutiveFailures = 3
)

$ErrorActionPreference = 'Stop'
$ServiceName = 'SoftafriqueBackupAgent'
$StatusFile = Join-Path $AgentDir 'status.json'

# Exit codes, matched by the RMM alert and by anything else watching this.
#   0 healthy
#   1 unhealthy
#   2 not enrolled
#   3 nothing to check yet (fresh install)
$exitHealthy = 0; $exitUnhealthy = 1; $exitNotEnrolled = 2; $exitUnknown = 3

$problems = New-Object System.Collections.Generic.List[string]

function Add-Problem([string]$Message) { $problems.Add($Message) }

if (-not (Test-Path $StatusFile)) {
    Write-Output "status=unknown reason=no_status_file path=$StatusFile"
    Write-Output "The agent has not written $StatusFile yet. If the service was only just installed, wait a minute and run this again."
    exit $exitUnknown
}

try {
    $st = Get-Content $StatusFile -Raw | ConvertFrom-Json
} catch {
    Write-Output "status=unhealthy reason=unreadable_status_file error=$($_.Exception.Message)"
    exit $exitUnhealthy
}

# ---- identity and enrollment ------------------------------------------------
if (-not $st.enrolled) {
    Write-Output "status=not_enrolled device=$($st.device_id) gateway=$($st.server_status)"
    Write-Output "This machine has no credentials. Re-run install.ps1 with a fresh one-time token."
    exit $exitNotEnrolled
}

$device = $st.device_id
$summary = "device=$device host=$($st.hostname) os=$($st.os_caption) tenant=$($st.tenant) gateway=$($st.server_status)"

# ---- the service ------------------------------------------------------------
$svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if (-not $svc) {
    Add-Problem "the $ServiceName service is not installed"
} elseif ($svc.Status -ne 'Running') {
    Add-Problem "the $ServiceName service is $($svc.Status), not Running"
}

# ---- the gateway's view -----------------------------------------------------
# A suspended device is a deliberate act by an operator. It is not "unhealthy"
# in the sense of broken, but it is not protected either, and a dashboard that
# treats it as fine is the exact failure this script exists to prevent.
if ($st.server_status -and $st.server_status -ne 'active') {
    Add-Problem "the gateway reports this device as '$($st.server_status)'"
}
if ($st.action_required) {
    Add-Problem "the agent needs a human: $($st.action_required)"
}

# ---- has it actually backed up ----------------------------------------------
$thresholdHours = $MaxHoursSinceSuccess
if ($thresholdHours -le 0) {
    # Three missed schedules, from the interval the agent is actually running
    # with (the gateway may have changed it), but never tighter than two hours so
    # a 15-minute schedule is not alerted on after 45 minutes.
    $scheduleHours = 1
    if ($st.schedule_interval_seconds -and $st.schedule_interval_seconds -gt 0) {
        $scheduleHours = [double]$st.schedule_interval_seconds / 3600
    }
    $thresholdHours = [Math]::Max(2, [Math]::Round($scheduleHours * 3, 2))
    Write-Verbose "no -MaxHoursSinceSuccess given; using 3 x $scheduleHours h = $thresholdHours h"
}

$successAgeHours = $null
if ($st.last_success) {
    try {
        $then = [datetimeoffset]::Parse($st.last_success)
        $successAgeHours = [Math]::Round(([datetimeoffset]::Now - $then).TotalHours, 2)
    } catch {
        Add-Problem "last_success '$($st.last_success)' is not a parseable timestamp"
    }
} else {
    Add-Problem 'this device has never completed a successful backup'
}

if ($null -ne $successAgeHours -and $successAgeHours -gt $thresholdHours) {
    Add-Problem "the last successful backup was $successAgeHours hours ago (threshold ${thresholdHours}h)"
}

if ($MaxConsecutiveFailures -gt 0 -and $st.consecutive_failures -ge $MaxConsecutiveFailures) {
    Add-Problem "$($st.consecutive_failures) consecutive failures; last error: $($st.last_attempt_error)"
}

# An attempt that has been "running" for far longer than any backup should take
# is a stuck restic, not a slow one.
if ($st.last_attempt_status -eq 'running' -and $st.last_attempt_start) {
    try {
        $started = [datetimeoffset]::Parse($st.last_attempt_start)
        $hours = ([datetimeoffset]::Now - $started).TotalHours
        if ($hours -gt 12) { Add-Problem "a backup has been 'running' for $([Math]::Round($hours,1)) hours and is probably stuck" }
    } catch { }
}

# ---- the one-line form, for grep and for RMM -------------------------------
# Deliberately one line and no colour, so a monitoring agent can match on it.
$repo = ''
if ($st.repo_stats) { $repo = " repo_bytes=$($st.repo_stats.repo_bytes) snapshots=$($st.repo_stats.snapshot_count)" }
$age = if ($null -eq $successAgeHours) { 'never' } else { "$successAgeHours" }

if ($problems.Count -gt 0) {
    Write-Output "status=unhealthy $summary last_attempt=$($st.last_attempt_status) last_success_hours=$age consecutive_failures=$($st.consecutive_failures)$repo"
    foreach ($p in $problems) { Write-Output "  - $p" }
    exit $exitUnhealthy
}

Write-Output "status=healthy $summary last_attempt=$($st.last_attempt_status) last_success_hours=$age consecutive_failures=$($st.consecutive_failures)$repo"
Write-Output "next_backup=$($st.next_backup_time) last_snapshot=$($st.last_snapshot_id) last_duration_s=$($st.last_duration_seconds) restic_duration_s=$($st.restic_duration_seconds)"
exit $exitHealthy
