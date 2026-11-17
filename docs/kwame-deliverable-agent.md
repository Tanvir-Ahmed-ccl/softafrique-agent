# Softafrique Backup Agent — deliverable sections for Kwame

Agent-side sections for the joint client-facing document. Merge these with the
gateway sections; the agent is complete at **0.2.36**, tag `v0.2.36`.

The operational runbook is [`OPERATIONS.md`](OPERATIONS.md). The exact wire
contract is [`gateway-contract.md`](gateway-contract.md); if this summary and the
Go structs disagree, the structs are the behaviour and this document is the bug.

---

## 1. Endpoint half of the design

### 1.1 Transport and identity

| Item | Value |
| --- | --- |
| API base | `https://backup.softafrique.net/api/v1` |
| Protocol | HTTPS only by default; a bare host is upgraded to `https://` |
| TLS | TLS 1.2 minimum; certificate verification cannot be disabled; no certificate pin yet |
| Authentication | HTTP Basic for `/config` and `/status`: username `device_id`, password `device_password` |
| Repository | `rest:https://backup.softafrique.net/{device_id}` returned by `/enroll` and passed to restic unchanged |
| Device identity | Enrollment-assigned `device_id`; never derived from the machine name; also used as restic `--host` on writes |
| Redirects | Refused across hosts, so the Basic password cannot be replayed to another host |
| Timeout | 30 seconds per request, including the response body; 10 seconds for the uninstall `POST /withdraw` |

The agent uses **five endpoints**: `POST /enroll`, `GET /config`,
`POST /status`, `POST /withdraw`, and `GET /health`. There is no `/heartbeat` endpoint.
"Heartbeat" is a cadence: an idle agent re-posts its current state with
`POST /status`.

### 1.2 `POST /enroll` — no auth, one-time token

Request:

```json
{
  "token": "smtl_9f2c1d4e7a8b4c0f",
  "hostname": "FILESERVER01",
  "os_caption": "Windows Server 2019 Standard",
  "backup_path": "D:\\CustomerData",
  "agent_version": "0.2.36"
}
```

Reply:

```json
{
  "tenant": "acme",
  "device_id": "dev-01HQ8K2M4P",
  "repository": "rest:https://backup.softafrique.net/dev-01HQ8K2M4P",
  "device_password": "4Jt8sQ2vNx1pLmZa",
  "encryption_key": "Ym9ndXMtZW5jcnlwdGlvbi1rZXktZG8tbm90LXVzZQ",
  "backup_path": "D:\\CustomerData"
}
```

`device_id`, `repository`, `device_password` and `encryption_key` are mandatory.
A reply missing any of them is rejected without writing anything. The device id
must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`; the repository URL must not
contain `user:pass@`.

The gateway creates the repository at enrollment and fails enrollment if its own
`init` fails. The agent contains no `restic init` path. It also has no `forget` or
`prune` path: the repository is append-only from the endpoint.

Before saving credentials, the agent opens the repository with the issued key and
checks `/config`. If the gateway is unreachable but the repository opens,
enrollment continues with a warning; a rejected `/config` or a bad key fails
enrollment while the one-time token is still in hand, rather than on tomorrow's
backup.

### 1.3 `GET /config` — HTTP Basic

Reply:

```json
{
  "device_id": "dev-01HQ8K2M4P",
  "backup_path": "D:\\CustomerData",
  "status": "active",
  "schedule_interval": "1h",
  "retry_interval": "15m"
}
```

`schedule_interval` and `retry_interval` accept either a Go duration string
(`"1h"`, `"15m"`) or a bare number of seconds. Omitted or zero leaves the local
value unchanged.

Only the exact value `active` permits a backup. Any other value is treated as
suspended, including an empty or missing `status`. A successful reply that says
the device is not active is a deliberate operator decision.

| Gateway state | How it arrives | Agent behaviour |
| --- | --- | --- |
| `active` | `200` + `{"status":"active"}` | Backups may run |
| `suspended` | `200` + `{"status":"suspended"}` | No backup; local status `suspended`; waits for an operator |
| `revoked` | `403` | No backup; local status `revoked`; requires re-enrollment |
| `archived` | `403` | Treated exactly like `revoked` |

A transport failure or `5xx` on `/config` does **not** stop backups. The last
known configuration is kept, including the cached `remote.json`, so a device that
boots without a network still knows its folder. A `401` or `403` is terminal:
the device marks itself revoked and asks for re-enrollment.

### 1.4 `POST /status` — HTTP Basic

Sent after every completed attempt, and on the heartbeat timer while idle. The
body is exactly nine fields:

```json
{
  "last_backup_status": "success",
  "last_snapshot_id": "a1b2c3d4",
  "files_added": 1284,
  "files_changed": 12,
  "bytes_added": 734003200,
  "last_duration_seconds": 41.2,
  "last_backup_error": "",
  "agent_version": "0.2.36",
  "os_caption": "Windows Server 2019 Standard"
}
```

- `last_backup_status` is only `success` or `failed`.
- A run that made no attempt sends nothing: suspended, revoked, unenrolled and
  refused-folder runs are recorded in `status.json`, not reported as failures.
  The idle heartbeat still posts the current state separately.
- A recreated empty folder is reported as `failed` with the reason, because
  reporting `success` would claim data was protected when no snapshot was taken.
- `last_duration_seconds` is wall-clock time including network wait; restic's own
  duration is local-only in `status.json`.
- The agent ignores the response body, so any `2xx` is success.
- `401` or `403` on this call also means the device has been withdrawn.

### 1.5 `GET /health` — no auth

Any `2xx` is healthy. A failure is logged as a warning and never blocks
enrollment or a backup. The gateway API and the repository are separate
services; one being unreachable must not stop data protection.

### 1.6 Status-code map

| Code | `/enroll` | `/config` | `/status` |
| --- | --- | --- | --- |
| `2xx` | Credentials issued | Device may back up | Report accepted |
| `400` | Payload rejected; message shown to the technician | Last known config kept | Report rejected; backup continues |
| `401` | Avoid; pre-enrollment credentials are meaningless | Device marked revoked | Device marked revoked |
| `403` | Token invalid, expired or spent | Device marked revoked | Device marked revoked |
| `404` | Rejected | Last known config kept | Report rejected; backup continues |
| `409` | Already enrolled | Not expected | Report rejected; backup continues |
| `5xx` | Retried later | Last known config kept; backups continue | Report rejected; backup continues |

### 1.7 Not on the wire today

- **Uninstall withdrawal — implemented.** `POST /withdraw` is authenticated with
  the device's own credentials, idempotent, called with a 10-second timeout while
  `credentials.dat` still exists, and best effort: an unreachable gateway leaves
  a line in the log and local removal carries on. The MSI and `uninstall.ps1`
  both call it before deleting the key. It cannot delete the repository, key or
  snapshots; reactivation is the gateway admin's
  `/api/v1/admin/device/{device_id}/reenroll`. `agent unenroll` deliberately does
  not call it, so the unenroll → enroll path still works.
- **Repository usage in `/status`.** It stays in `status.json` because a usage
  figure without `captured_at` cannot be distinguished from a stale one.
- **`reimport`.** Entirely local; it never contacts the gateway. It exists for a
  reimaged machine where the DPAPI blob is gone and the operator has the
  escrowed key. Entitlement is proven only by the repository accepting the key.

---

## 2. Agent architecture

### 2.1 Process model

One Windows executable provides both the service and the CLI. The service control
manager starts it with no arguments, so no arguments means service mode rather
than a usage error.

- The service runs as **LocalSystem**, auto-start with delayed start.
- A named mutex `Global\SoftafriqueBackupAgent.SingleInstance` prevents two
  write-side processes from backing up to the same repository under the same
  `--host`.
- Read-only commands (`status`, `snapshots`, `restore`, `selftest`) deliberately
  do not take the mutex, so a technician can restore during a backup.
- The scheduled loop, configuration poller, heartbeat and usage reading run in
  one process. The scheduler derives the next run from the persisted
  `last_success`, so a restart or reboot resumes the correct schedule.

### 2.2 Data flow

```text
dashboard issues one-time token
        |
        v
agent enroll  --->  POST /enroll
        |               returns tenant, device_id, repository,
        |               device_password, encryption_key, backup_path
        v
credentials.dat  (DPAPI machine scope + ProgramData ACL)
        |
        +--> restic REST repository: RESTIC_REST_USERNAME/PASSWORD, RESTIC_PASSWORD
        +--> GET /config  (HTTP Basic device_id/device_password)
        +--> POST /status (HTTP Basic device_id/device_password)

scheduled attempt:
  load credentials -> refresh /config -> apply path policy -> create missing
  folder -> restic backup with --host device_id -> record status.json ->
  POST /status -> refresh repository stats
```

### 2.3 Components

| Component | Responsibility |
| --- | --- |
| `cmd/agent` | CLI, service entry, enrollment, backup, restore, status, diagnostics |
| `cmd/validatepath` | Standalone folder validator called by the MSI |
| `internal/agent` | Orchestration: enrollment, config sync, backup attempts, reporting, recovery |
| `internal/api` | Gateway client and error classification |
| `internal/config` | YAML config, defaults, validation, installer rendering |
| `internal/identity` | Hostname and OS caption, including Server 2016/2019 detection |
| `internal/lock` | Single-instance named mutex |
| `internal/pathpolicy` | Fixed-disk, network-share and system-directory policy shared by installer and agent |
| `internal/restic` | restic invocation, exit-code handling, snapshot/stat/restore operations |
| `internal/scheduler` | Due-time calculation, jitter, retries, attempt bookkeeping |
| `internal/secret` | DPAPI-sealed credentials store |
| `internal/service` | Windows service wrapper |
| `internal/status` | `status.json` schema v2, atomic writes, 0.1.0 migration |
| `installer/` | WiX v5 MSI |
| `scripts/rmm/` | Tactical RMM install, health-check and uninstall scripts |

### 2.4 Scheduling, retries and state

The shipped configuration uses:

| Setting | Shipped value |
| --- | --- |
| Backup interval | 1 hour |
| Retry interval | 5 minutes |
| Config poll | 5 minutes |
| Idle status heartbeat | 15 minutes |
| Startup/retry jitter | up to 30 seconds |
| Maximum retries | unlimited (`-1`) |
| Repository stats | every 30 minutes while idle |

- A device that has never succeeded runs immediately.
- A successful run waits for the effective schedule interval.
- A failed run retries with linear backoff capped at five times the retry
  interval, plus equal jitter, so a recovering fleet does not synchronise.
- A terminal failure is not retried within the same run: revoked credentials, a
  missing repository, or a refused path.
- A recreated empty folder ends the attempt, but the device rechecks it at the
  retry interval. No snapshot is taken until the customer's data is back.
- `next_backup_time` is written for monitoring, but it is a plan, not a promise.

### 2.5 Configuration precedence

For the protected folder:

1. An explicit `include:` in `config.yaml` wins. This is the support override.
2. Otherwise the `backup_path` from `GET /config`.
3. Otherwise the `backup_path` from the `/enroll` reply.

An empty result is an error. The agent never defaults to a drive root.

For schedules, the gateway's `schedule_interval` and `retry_interval` override the
local values. The last successful remote configuration is cached in
`remote.json`, so an offline boot still knows the folder and schedule.

The config file contains **no credentials**. The device id in an enrolled device
comes from the sealed credentials, not from `config.yaml`. The single exception is
an unenrolled/break-glass setup using `repo`, `password_file` and `device_id`.

### 2.6 Backup, restore and statistics

- restic is bundled beside the agent; the customer machine needs nothing else.
- Backups are incremental and append-only. Each run checks the whole protected
  set and adds a snapshot of what changed.
- The repository password is the enrollment `encryption_key`; the REST backend's
  HTTP Basic password is `device_password`. They are separate secrets.
- New snapshots carry `--host <device_id>`. Listing, stats, ping and restore do
  **not** pass `--host`, so history written under an older hostname remains
  visible.
- `agent restore` is read-only, does not take the single-instance mutex, and
  restores `latest` into `C:\SoftafriqueRestore` by default.
- `repo_stats.repo_bytes` is whole-repository raw data; `restore_bytes` and
  `file_count` describe the latest snapshot. `file_count` is restic's node
  count, not a pure file count: directories count as nodes, so a folder with
  four files and three subfolders reads 8. The field stays as-is for schema
  compatibility; the dashboard must label it "items", not "files".

### 2.7 Failure semantics

`last_attempt_status` is one of:

| Value | Meaning |
| --- | --- |
| `running` | An attempt started and has not finished |
| `success` | A snapshot was produced |
| `failed` | The attempt ended in an error |
| `suspended` | No attempt was made because the gateway says the device is not active |
| `recreated` | The protected folder was missing; the agent recreated it and took no snapshot |

`last_success` only moves after a real snapshot. A week of failures leaves the
last genuine success date intact, so "how long since this customer was actually
protected" remains answerable.

---

## 3. Service and security model

### 3.1 Windows service

| Item | Value |
| --- | --- |
| Service name | `SoftafriqueBackupAgent` |
| Display name | Softafrique Backup Agent |
| Account | `LocalSystem` |
| Start | Automatic, delayed auto-start |
| Recovery | Restart on first and second failure after 60 seconds; no third action |
| Install behaviour | The MSI creates and starts the service; without a token it stays running and waits for enrollment |
| Binary | `C:\Program Files\Softafrique Backup Agent\SoftafriqueBackupAgent.exe` |
| State | `C:\ProgramData\SoftafriqueBackupAgent\` |
| Install registry | `HKLM\SOFTWARE\Softafrique\BackupAgent` (`InstallDir`, `DataDir`) |

LocalSystem is required because backups run while nobody is signed in and because
the data directory is restricted to SYSTEM and Administrators.

### 3.2 Two secrets, stored once

| Secret | Used for | Storage |
| --- | --- | --- |
| `device_password` | HTTP Basic for `/config` and `/status`; restic REST Basic password | `credentials.dat` |
| `encryption_key` | restic repository password | `credentials.dat` |

`credentials.dat` is sealed with **DPAPI machine scope** and a fixed application
entropy value. The blob is written atomically and is never logged. Machine scope
is deliberate: a user-scoped blob could not be read by the LocalSystem service.

DPAPI alone is not the boundary. Anything running as SYSTEM or an administrator
can decrypt a machine-scope blob. The actual boundary is the data-directory ACL:
full control for SYSTEM and Administrators, inheritance disabled, nothing else.
The MSI applies that ACL and re-propagates it to existing children on upgrade.
The agent logs a warning if `data_dir` points somewhere other than the standard
location, because that directory will not have the same protection.

On an enrolled device, restic receives both secrets through its **child-process
environment** (`RESTIC_PASSWORD`, `RESTIC_REST_USERNAME`, `RESTIC_REST_PASSWORD`),
not through a command line and not in a password file. The break-glass
`-password-file` option is the exception. Command lines are visible to local
processes and are frequently logged by RMM.

### 3.3 Enrollment token handling

- The preferred input is `SOFTAFRIQUE_ENROLL_TOKEN`, then `-token-file`, then
  `-token` as a last resort with a warning.
- The MSI accepts only `ENROLLTOKENFILE`, the **path** to a token file. It never
  accepts the token itself as an MSI property, because MSI properties are written
  to the install log.
- A token file readable by an unprivileged account is refused, with the `icacls`
  command needed to fix it.
- After a successful enrollment, the agent deletes a consumed token file.
- The one-time token does not survive into `config.yaml`, the install directory,
  `status.json` or the log.

### 3.4 Append-only repository

The gateway is append-only. The agent has no `forget`, `prune` or delete code
path; a test reads the source to assert that retention commands never appear in
restic arguments. A compromised endpoint can add snapshots and read its own
history, but cannot delete history. Retention is a server-side decision made from
a trusted machine with a different credential.

The consequence is stated plainly: until a server-side retention job exists,
storage grows without bound. That is the safe direction to be wrong in.

### 3.5 Network and data protection

- HTTPS is required by default; a bare host is upgraded to `https://`.
- TLS 1.2 is the floor, so Windows Server 2016/2019 works without .NET registry
  changes. Certificate verification is always on.
- Cross-host redirects are refused so the Basic password is not forwarded.
- There is no certificate pin yet. A machine trusting a rogue CA could be
  intercepted. This is a known limitation.
- `status.json` contains no secrets: no password, no key, no repository URL; only
  the repository host name.
- Logs use UTC, so agent and gateway records line up across DST changes.

### 3.6 What a compromised endpoint can and cannot do

| Capability | Result |
| --- | --- |
| Read the local sealed key as SYSTEM/Administrator | Possible; the ACL is the intended boundary |
| Add snapshots to its own repository | Possible |
| Delete snapshots or repository history | Not possible; no delete/retention path |
| Read another device's repository | Not possible without that device's key and credentials |
| Keep using credentials after gateway revocation | No; `/config` and `/status` return `403` and backups stop |
| Reuse a spent enrollment token | No; the gateway returns `403` |

### 3.7 Removal and withdrawal

The MSI uninstall removes the service, binaries, sealed credentials and local
state (`config.yaml`, `status.json`, `remote.json`, `agent.log`, restic cache).
It leaves the customer's protected folder and the server-side repository
untouched, so history survives and a reinstall can restore it with the same
escrowed key.

Uninstall also withdraws the device at the gateway first, while the credentials
still exist, so it stops being enrolled. The call is best effort: an offline
machine still loses its local key, and the device can then be withdrawn from the
dashboard. Withdrawal cannot delete the repository, the escrowed key or any
snapshot, and reactivation is an admin action at
`/api/v1/admin/device/{device_id}/reenroll`.

### 3.8 Signing status

The current pilot MSI and executables are **unsigned** while the signing
certificate secrets are not configured. Customer sites with
SmartScreen or AppLocker may show an unknown-publisher warning or block the
install. Code signing is a GA gate, not an engineering unknown.

The CI workflow is ready to sign once the certificate exists. On a tag build,
`scripts/sign-windows.ps1` signs `SoftafriqueBackupAgent.exe` and
`validatepath.exe` before they are packed, then signs the MSI and runs
`signtool verify /pa` on every file. It is inactive while the secrets are absent
and fails the build if a configured certificate cannot sign or verify.

Configure these GitHub Actions secrets:

- `SOFTAFRIQUE_SIGNING_PFX_BASE64`: the PFX file, base64-encoded.
- `SOFTAFRIQUE_SIGNING_PFX_PASSWORD`: the PFX password.

The script uses SHA-256 and an RFC 3161 timestamp. It imports the PFX into the
runner's certificate store and selects it by thumbprint, so the PFX password is
never on a command line. It removes the imported certificate and the decoded PFX
in a `finally` block. It signs Softafrique's own binaries and MSI. It
deliberately does **not** re-sign `restic.exe`: that is third-party code, and
re-signing it with Softafrique's certificate would assert authorship of a binary
Softafrique did not build.

The manual equivalent is:

```powershell
signtool sign /fd SHA256 /tr http://timestamp.digicert.com /td SHA256 `
  /f softafrique-code-signing.pfx /p $env:CERT_PASSWORD `
  SoftafriqueBackupAgent.msi `
  SoftafriqueBackupAgent.exe validatepath.exe
signtool verify /pa /v SoftafriqueBackupAgent.msi
```

The signed tag build is installed and uninstalled by the same workflow, so the
smoke test covers the bytes that are published. Signing is inactive until the
secrets exist; the canonical MSI is unsigned while they are absent. The release
job computes `SHA256SUMS` from the exact MSI it uploads, so signing changing the
hash cannot leave a stale checksum behind.

---

## 4. Installer and deployment process

### 4.1 Artefact

The canonical artefact is the GitHub release asset for the tag:

- Tag: `v0.2.36`
- Release: https://github.com/Tanvir-Ahmed-ccl/softafrique-agent/releases/tag/v0.2.36
- Asset: `SoftafriqueBackupAgent-0.2.36.msi`
- Checksum: `SHA256SUMS` on the same release

The tag build publishes the MSI and `SHA256SUMS` from the same job, after signing
when the certificate secrets are configured. The hash is taken from the exact
bytes uploaded, so it is not copied from an earlier run or from this document.
Deployments should pin the release and verify the hash, not pull “the latest
artifact in Actions”.

The MSI is WiX v5, x64, per-machine, and carries the agent, `restic.exe` and
`validatepath.exe`. It is unsigned in this release.

### 4.2 MSI properties

| Property | Purpose | Default |
| --- | --- | --- |
| `BACKUPPATH` | Customer folder to protect | `C:\SoftafriqueBackup` |
| `ALLOWUNC` | Permit a network share deliberately (`1`) | refused |
| `ENROLLTOKENFILE` | Path to a one-time token file; enrolment runs before the service starts | none |
| `TESTINSTALL` | CI-only layout test; skips folder validation and enrolment | none |

`BACKUPPATH` is validated by `validatepath.exe` before `config.yaml` is written.
A path with a trailing separator is normalised safely. The installer checks the
folder but does not create it; the service creates it as LocalSystem on the first
backup, so its owner and ACL are those of the process that will actually write
it. Pre-create a customer-owned folder first if interactive users also need
access.

The wizard asks **one** folder question. There is no install-location page, so a
technician cannot install the service into `C:\Windows` from the wizard.
Maintenance-mode Change is disabled. The supported way to change the protected
folder is the dashboard, which sends a new `backup_path` on the next `/config`
poll.

### 4.3 Install sequence

1. `ValidateBackupFolder` runs after files are laid down and before any config is
   written. It refuses a non-fixed disk, a share without opt-in, a drive/share
   root, a system directory, or the agent's own data directory.
2. `render-config` writes `config.yaml` from the template, filling in the
   protected path and state paths. An existing config is preserved on repair or
   upgrade, so an on-site edit is not discarded.
3. The data-directory ACL is applied, and existing children are reset to inherit
   it.
4. If `ENROLLTOKENFILE` is set, enrollment runs before `StartServices`, so the
   first service start already has credentials.
5. `StartServices` starts the service. Without a token the service stays running
   and waits for enrollment; exiting would fail the install with error 1920.
6. The install-created marker is cleared only after a successful start. A
   rollback removes a config this transaction created, without deleting a config
   that existed before.

Major upgrades use `RemoveExistingProducts` **after** `InstallFinalize`. The new
package must install successfully before the old one is removed, so a failed
upgrade cannot take a working customer offline. The sealed credentials are
preserved across a major upgrade.

### 4.4 RMM deployment

The supported RMM path is:

```powershell
$env:SOFTAFRIQUE_ENROLL_TOKEN = $rmmVar
.\install.ps1 -MsiPath C:\Temp\SoftafriqueBackupAgent.msi -BackupPath "D:\CustomerData"
```

`install.ps1`:

1. requires `-MsiPath` and `-BackupPath`;
2. checks the volume type before changing anything, giving a sentence rather than
   an msiexec exit code for a DVD or removable drive;
3. installs the MSI silently with `BACKUPPATH` and a verbose MSI log;
4. for a share, requires `-AllowUNC`, passes `ALLOWUNC=1`, and writes
   `allow_unc: true` into `config.yaml` so the installer and the running agent
   agree;
5. stops the service started by the MSI, enrolls with the token in the process
   environment, then starts the service again;
6. waits for `status.json` to say `enrolled=true` and prints the device id,
   tenant, repository host, protected paths and gateway status.

The token is never on a command line and is removed from the process environment
after enrollment.

### 4.5 Manual install

Interactive:

```powershell
msiexec /i SoftafriqueBackupAgent.msi
```

Silent:

```powershell
msiexec /i SoftafriqueBackupAgent.msi /qn BACKUPPATH="D:\CustomerData"
$env:SOFTAFRIQUE_ENROLL_TOKEN = "<one-time token>"
& "C:\Program Files\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" enroll
Remove-Item Env:\SOFTAFRIQUE_ENROLL_TOKEN
Start-Service SoftafriqueBackupAgent
```

For a token file:

```powershell
msiexec /i SoftafriqueBackupAgent.msi /qn BACKUPPATH="D:\CustomerData" ENROLLTOKENFILE="C:\Temp\token.txt"
```

The agent deletes the token file after a successful enrollment. If the file is
readable by an unprivileged account, enrollment refuses and prints the `icacls`
command needed to secure it.

### 4.6 Upgrade and uninstall

- A new MSI must carry a higher `ProductVersion` than the installed one.
- CI checks that the git tag, source defaults and compiled MSI `ProductVersion`
  all agree, and that the version differs from the previous release.
- A failed upgrade leaves the old working installation in place.
- A standalone uninstall withdraws the device at the gateway, then removes local
  credentials and state but leaves the protected folder and repository untouched.
  A gateway failure never fails the uninstall.
- A 0.1.0 device is not enrolled on the gateway; upgrading it is an
  **enrollment**, not a credential-preserving upgrade. Use a fresh token.

### 4.7 What CI proves on every change

- Go tests and vet, including `GOOS=windows` vet and an arm64 cross-build.
- The suite natively on `windows-latest`.
- `validatepath.exe` against a real filesystem: missing folder, file-not-folder,
  drive root, unmounted volume, network-share refusal.
- A real MSI install with `BACKUPPATH` including a trailing separator.
- The service is registered and running after install.
- `config.yaml` is written with placeholders substituted, `allow_unc: false`, and
  the exact protected path.
- A repair preserves an on-site config edit.
- A refused network share and a refused `C:\Windows` path both fail installation
  and leave no poisoned config behind.
- A good install immediately after a refused one protects the new path.
- Uninstall removes the service, binaries, credentials and stale state.
- The compiled MSI tables assert `BackupPathDlg` ordering, no `InstallDirDlg`,
  `ValidateBackupFolder` before `WriteConfig`, and `RemoveExistingProducts` after
  `InstallFinalize`.
- The compiled MSI `ProductVersion` matches the tag.

This is the install smoke test Mohsin asked for. It is not a substitute for a
human walking the wizard on a real Windows host before release.

### 4.8 Deployment checklist

- [ ] Confirm the device is a supported Windows build; the pilot is validated on
      Windows Server 2019 and Windows 10.
- [ ] Confirm the protected folder is on a fixed disk, or deliberately opt in to
      a network share whose machine account already has access.
- [ ] Confirm the folder is not `C:\Windows`, `C:\ProgramData`, a drive root, or
      the agent data directory.
- [ ] Issue a one-time token and deliver it through an ACL-protected file or the
      RMM process environment, never a command line.
- [ ] Verify the release SHA256 before installing.
- [ ] After install, confirm the service is Running and `status.json` says
      `enrolled=true`.
- [ ] Wait for the first successful snapshot and check `last_success` and
      `repo_stats.snapshot_count`.
- [ ] Register the device in the monitoring dashboard and set the expected
      staleness threshold from `schedule_interval_seconds`.

---

## 5. Command reference and exit codes

### 5.1 Common flags

Most commands accept:

| Flag | Meaning |
| --- | --- |
| `-config <path>` | Config file; defaults to `%ProgramData%\SoftafriqueBackupAgent\config.yaml` |
| `-repo <url>` | Repository override for an unenrolled/break-glass device |
| `-password-file <path>` | restic password file for an unenrolled device |
| `-include <folders>` | Comma-separated protected-folder override |
| `-device-id <id>` | Device id for an unenrolled/break-glass device |
| `-verbose` | Debug logging |

Secrets should be passed through the environment or ACL-protected files, not
command lines.

### 5.2 Commands

| Command | Purpose | Important flags / behaviour |
| --- | --- | --- |
| `agent` with no arguments | Windows service entry point | Used by the SCM; not normally run by a technician |
| `enroll` | Exchange a one-time token for credentials | `-token`, `-token-file`; environment `SOFTAFRIQUE_ENROLL_TOKEN` wins over both; deletes a consumed token file |
| `unenroll` | Delete local credentials | `-yes` required; `-force` deletes an unreadable blob; does not contact the gateway or touch the repository |
| `withdraw` | Tell the gateway this device is decommissioned | Best effort, 10-second timeout, used by uninstall while credentials exist; never fails the uninstall; does not delete local credentials |
| `reimport` | Rebuild credentials after a reimage | `-key-file`, `-device-password-file`, and a device id from config or `-device-id`; repository comes from local config, not the gateway |
| `run` | Run the scheduled loop in the foreground | Takes the single-instance lock |
| `service` | Explicit service entry point | Same loop as `run`, under the SCM |
| `install` | Install the Windows service manually | The MSI normally does this |
| `uninstall` | Remove the Windows service manually | Leaves credentials and data alone |
| `backup` | Run one backup now | Takes the single-instance lock; records the attempt like a scheduled run |
| `selftest` / `doctor` | Check repository, key and gateway | `-v` prints paths and device identity; gateway health failure is only a warning |
| `snapshots` | List every recovery point | `-latest` shows only the newest; no `--host` filter, so old history is visible |
| `status` | Print `status.json` | `-short` prints one monitoring line; reads enrollment from the credentials blob |
| `restore` | Restore a snapshot | `restore <id>` or `-snapshot`; `-target` (absolute); repeatable `-include`; no write lock |
| `render-config` | Installer helper: render `config.yaml` | Internal; `-template`, `-out`, `-backup-path` |
| `commit-rendered-config` | Installer rollback helper | Internal |
| `discard-rendered-config` | Installer rollback helper | Internal |
| `purge-data` | Remove local state | Internal; used by the MSI uninstall |
| `version` | Print the build version | |
| `help` | Print usage | |

Examples:

```powershell
& $Agent version
& $Agent status -short
& $Agent selftest -v
& $Agent snapshots
& $Agent backup
& $Agent restore a1b2c3d4 -target C:\SoftafriqueRestore
& $Agent restore -snapshot latest -target D:\Restore\CustomerData
& $Agent status -config D:\Temp\config.yaml
```

`restore` accepts the snapshot id as a positional first argument or through
`-snapshot`. The target must be absolute; the default is
`C:\SoftafriqueRestore`. Restore never modifies the repository.

### 5.3 Agent CLI exit codes

| Code | Meaning |
| --- | --- |
| `0` | Success |
| `1` | Generic error |
| `2` | Bad command line or unknown command |
| `3` | Not enrolled: no credentials |
| `4` | Action required: a human must intervene (re-enroll, missing folder, lock conflict) |
| `5` | Reserved for “unhealthy”; not currently returned by the agent CLI |

`health-check.ps1` has its own exit codes because it is the monitoring contract:

| Code | Meaning |
| --- | --- |
| `0` | Healthy |
| `1` | Unhealthy |
| `2` | Not enrolled |
| `3` | Nothing to check yet: no `status.json` |

`install.ps1` and `uninstall.ps1` return `0` on success and `1` on failure.

`validatepath.exe` returns:

| Code | Meaning |
| --- | --- |
| `0` | Usable; it may not exist yet |
| `2` | Empty path |
| `3` | Path missing and its volume is missing |
| `4` | Path exists but is a file, not a folder |
| `5` | Drive/share root, system directory, or agent data directory |
| `6` | Cannot read or list the path |
| `7` | Not on a fixed disk, or a network share without opt-in |

---

## 6. Technician troubleshooting guide

### 6.1 First five checks

```powershell
Get-Service SoftafriqueBackupAgent
Get-Content "$env:ProgramData\SoftafriqueBackupAgent\status.json" -Raw | ConvertFrom-Json
Get-Content "$env:ProgramData\SoftafriqueBackupAgent\agent.log" -Tail 50
& "C:\Program Files\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" status -short
& "C:\Program Files\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" selftest -v
```

Also run the monitoring script, which is the same check the RMM alert uses:

```powershell
.\scripts\rmm\health-check.ps1
```

It prints one `status=...` line and exits `0` healthy, `1` unhealthy, `2` not
enrolled, or `3` no status file yet.

### 6.2 Reading `status.json`

| Field | Interpretation |
| --- | --- |
| `enrolled` | `false` means no credentials; nothing is protected. `status -short` reads the blob, not the stale file |
| `server_status` | Gateway view: `active`, `suspended`, `revoked`, `unknown` |
| `action_required` | Human-readable next action. If set, read it first |
| `last_attempt_status` | `running`, `success`, `failed`, `suspended`, `recreated` |
| `last_attempt_start` / `last_attempt_end` | `health-check.ps1` flags a `running` attempt older than 12 hours as probably stuck |
| `last_success` | When data was last actually protected; the field that matters |
| `last_attempt_error` | Why the last attempt failed |
| `consecutive_failures` | Drives the failing alert |
| `last_duration_seconds` | Wall clock including network wait; quote this to a customer |
| `restic_duration_seconds` | restic's own duration; do not quote as backup duration |
| `backup_paths` | The paths actually handed to restic |
| `repo_host` | Repository host only; never a URL or credential |
| `repo_stats.repo_bytes` | Whole repository, including history |
| `repo_stats.restore_bytes` | Logical size of the latest snapshot |
| `repo_stats.file_count` | restic node count: files + directories + snapshot root; label it “items”, not “files” or “files protected” |
| `repo_stats.snapshot_count` | Number of recovery points |
| `repo_stats.captured_at` | When the usage reading was taken; stale readings are identifiable |
| `schedule_interval_seconds` | Effective schedule after gateway override; use for staleness thresholds |
| `next_backup_time` | Planned next run; empty immediately after enrollment is normal |

`status.json` is readable only by SYSTEM and Administrators. A non-elevated
monitor cannot read it and must be given elevation or an administrator must copy
the file out.

### 6.3 Symptom table

| Symptom | First check | Usual fix |
| --- | --- | --- |
| `status=not_enrolled` | `enrolled` in `status.json`; token used? | Re-run `install.ps1` with a fresh token |
| Service not running | `Get-Service`; Application event log | Check `config.yaml`, `credentials.dat`, `restic.exe`; start service |
| `server_status=suspended` | `action_required` | Operator must set the device active in the dashboard |
| `action_required` says re-enroll | `revoked` or rejected credentials | New token; `install.ps1`; repository and snapshots remain |
| `last_attempt_status=recreated` | `action_required` names the folder | Site visit; restore the customer's data; do not accept an empty folder |
| Backups stopped during a gateway outage | `last_attempt_error` | Transport/5xx should not stop backups; a 401/403 should, by design |
| `last_success` old but service running | `consecutive_failures`, `last_attempt_error` | Read the log; run `selftest`; check repository reachability |
| Backup slow, dashboard says fast | Compare `last_duration_seconds` and `restic_duration_seconds` | Quote wall clock; network wait is real |
| Install fails with `1603` | MSI log in `%TEMP%` or RMM job | Look for the deferred action message; test path with `validatepath.exe` |
| Install refuses `D:\...` | `validatepath.exe` exit `7` | D: may be optical/removable; choose a folder on a fixed disk |
| Install refuses a share | `validatepath.exe` exit `7` | Use a fixed disk, or opt in deliberately with `-AllowUNC` and grant the machine account access |
| Install refuses `C:\Windows` | `validatepath.exe` exit `5` | Choose the customer's data folder, not a system directory |
| Cannot read `status.json` | Elevation | Run as SYSTEM/Administrator or copy it out |
| Restore needed during a backup | `agent restore` | Restore is read-only and does not need the service stopped |
| Reimaged machine | Local `credentials.dat` gone | `agent reimport` with escrowed key, device password and device id; repository from local config |
| Uninstall left the device enrolled | Withdraw line in `agent.log` and the RMM job output | The machine was offline at uninstall; withdraw it from the dashboard |

### 6.4 “Backups stopped when the gateway went down”

They should not have. A network error or `5xx` on `GET /config` keeps the last
known configuration and continues backing up. `remote.json` also lets a device
boot without a network. The log will distinguish a transport failure from a
`401`/`403` rejection, which is terminal by design.

### 6.5 “It says re-enroll”

The gateway no longer accepts the device credentials, or has withdrawn the
device. Get a fresh one-time token and run `install.ps1` again. The repository,
snapshots and escrowed key are untouched; re-enrollment does not create a second
history.

### 6.6 “The protected folder was missing”

`last_attempt_status` is `recreated`. The agent recreated the folder but took no
snapshot, because an empty snapshot is not protection. Confirm whether the
customer's data is in the path named by `action_required`. If it is not, the
snapshots before the deletion are the only copy. The notice clears on the next
run that backs up real data.

### 6.7 Restore procedure

```powershell
& $Agent snapshots
& $Agent restore <snapshot-id> -target D:\Restore\CustomerData
```

The snapshot id can also be passed with `-snapshot`. The target must be absolute.
To restore only part of a snapshot, repeat `-include`:

```powershell
& $Agent restore latest -target D:\Restore -include "/CustomerData/Invoices/*"
```

Restore does not modify the repository and does not take the single-instance
lock, so the customer's scheduled backup can continue. If the machine was lost,
use `reimport` with the escrowed key and device id from the dashboard before
restoring.

### 6.8 Reimage and re-import

After a reimage, `credentials.dat` is gone but the gateway still has the device
and the escrowed key. The operator obtains the device id, key and device password
from the dashboard, writes the secrets to ACL-protected files, and runs:

```powershell
& $Agent reimport `
  -key-file C:\Temp\key.txt `
  -device-password-file C:\Temp\pass.txt `
  -device-id dev-01HQ8K2M4P `
  -config "$env:ProgramData\SoftafriqueBackupAgent\config.yaml"
```

Both secret files are required, and so is the device id: pass `-device-id` or set
`device_id` in the config, because the sealed blob that normally holds it is gone
after a reimage. The repository comes from local config, not the gateway, because
re-import exists for the case where the gateway is unreachable.
The command prints the repository and backup path and warns to confirm them: a
mistake here is silent until a restore fails. Delete both temporary secret files
afterwards.

### 6.9 Upgrade failure

A failed upgrade should leave the old working agent installed. The new package
must succeed before `RemoveExistingProducts` runs. Check:

```powershell
Get-WinEvent -LogName Application -MaxEvents 20 |
  Where-Object { $_.ProviderName -eq 'SoftafriqueBackupAgent' }
Get-Content "$env:ProgramData\SoftafriqueBackupAgent\agent.log" -Tail 100
```

If the old version is still installed and running, do not uninstall it to “start
clean”; fix the new package or the path and retry. Preserve
`credentials.dat`.

### 6.10 Information to collect for escalation

Collect, without secrets:

- device id, tenant, hostname and agent version from `status -short` or
  `status.json`;
- `status.json` itself (contains no credentials);
- the last 100 lines of `agent.log`;
- service state and start type;
- the exact `BACKUPPATH` and the output of `validatepath.exe` for that path;
- the MSI install log from `%TEMP%` or the RMM job;
- whether the gateway shows the device as active, suspended, revoked or archived.

Never send the encryption key, device password, `credentials.dat`, or a token
file. A restore needs the escrowed key, but support does not need it for triage.
