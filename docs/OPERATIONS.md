# Operations runbook

Everything an on-call engineer or a support technician needs. Written for the
0.2.0 release.

## Where things are

| Path | Contents |
| --- | --- |
| `C:\Program Files\Softafrique Backup Agent\` | `SoftafriqueBackupAgent.exe`, `restic.exe`, `validatepath.exe` |
| `C:\ProgramData\SoftafriqueBackupAgent\` | `config.yaml`, `status.json`, `agent.log`, `credentials.dat`, `remote.json` |
| `HKLM\SOFTWARE\Softafrique\BackupAgent` | InstallDir, DataDir |
| Service | `SoftafriqueBackupAgent`, auto (delayed), running as LocalSystem |

`credentials.dat` is the only file with anything sensitive in it, and it is
DPAPI-sealed in machine scope. Sealing is not the protection: anything running as
SYSTEM or an administrator can decrypt a machine-scope blob, so the directory ACL
is the boundary. The package grants full control to SYSTEM and Administrators,
inheritable, with inheritance switched off, and nothing else.

The practical consequence is that reading `status.json` needs elevation. The
Tactical RMM health check runs as SYSTEM and is unaffected. A third-party
monitor running as an ordinary user cannot open it, and must be given elevation
or have an administrator copy the file out. An earlier draft of the package
granted `Users` read on the directory to solve this; it did not work, because a
non-inheritable directory ACE does not reach the files inside it.

## Reading the status file

```powershell
Get-Content "$env:ProgramData\SoftafriqueBackupAgent\status.json" -Raw | ConvertFrom-Json
```

| Field | Read it as |
| --- | --- |
| `enrolled` | `false` means the device has no key. Nothing is being protected. |
| `server_status` | The gateway's view. Anything but `active` means backups are withheld. |
| `action_required` | Written for a human. If it is set, read it; it says what to do. |
| `last_attempt_status` | `running`, `success`, `failed` or `suspended`. `suspended` is not a failure: nothing was attempted. |
| `last_attempt_start` / `last_attempt_end` | Bracket the last attempt, so a `running` older than a few hours is a stuck restic. |
| `last_success` | When data was last actually protected. This is the field that matters. |
| `last_duration_seconds` | Wall clock, including waiting for the network. This is what to quote a customer. |
| `restic_duration_seconds` | restic's own number. Never quote this as the backup duration. |
| `consecutive_failures` | Drives the "failing" alert. |
| `repo_stats.captured_at` | When the usage numbers were read, so a stale figure is distinguishable. |
| `schedule_interval_seconds` | The effective interval after any gateway override. Use it for staleness thresholds. |

`last_backup_status` and `last_backup_time` are the 0.1.0 names for
`last_attempt_status` and `last_success`. They are kept so monitoring written
against 0.1.0 keeps working, and will be dropped in a future major version.

A 0.1.0 `status.json` is migrated on read: the `repo` field, which held the
repository URL with the password embedded in it, is dropped and the file is
rewritten as schema 2. The 0.1.0 `password_file` is deleted on upgrade if it
lives inside the data directory.

## Common situations

### "A machine says it is not backed up"

Work down this list; each step is cheap and rules something out.

1. `status.json` → `enrolled`. If `false`, the device has no credentials: re-run
   `install.ps1` with a fresh token.
2. `server_status`. If it is not `active`, an operator suspended it at the
   gateway. `action_required` usually says more.
3. `service` state. `Get-Service SoftafriqueBackupAgent`.
4. `last_attempt_error` and `consecutive_failures`.
5. `agent.log`, last 50 lines: `Get-Content "$env:ProgramData\SoftafriqueBackupAgent\agent.log" -Tail 50`.
6. `restic` reachability by hand:

```powershell
$env:RESTIC_PASSWORD = (read the key from the dashboard; do not echo it)
& "C:\Program Files\Softafrique Backup Agent\restic.exe" snapshots --host <device_id>
```

### "Backups stopped when the gateway went down"

They should not have. A `GET /config` failure on a network error or a 5xx leaves
the last known configuration in place and the backup proceeds, and `remote.json`
means a device that boots with no network still knows its folder. If you are
seeing otherwise, the log will say whether it was a rejection (401/403, which
*is* terminal by design) rather than a transport failure.

### "It says re-enroll"

The gateway no longer accepts the device's credentials, or has withdrawn it. This
is deliberately not retried. Get a new one-time token from the dashboard and run
`install.ps1` again; the repository and its snapshots are untouched.

### "The backup takes four minutes but the dashboard says one second"

Check `last_duration_seconds` against `restic_duration_seconds` in
`status.json`. The first is wall clock and is the number to quote. The second is
restic's own and excludes however long the run waited on the network. A gateway
that adds a minute of latency shows up only in the first.

### "The agent will not start"

```
Get-WinEvent -LogName Application -MaxEvents 20 | Where-Object { $_.ProviderName -eq 'SoftafriqueBackupAgent' }
sc query SoftafriqueBackupAgent
```

Common causes, in order: no `config.yaml` (the MSI writes it; a manual install
must supply one), `credentials.dat` unreadable (the ACL was changed by hand), or
`restic.exe` missing from the install folder.

## Upgrading 0.1.0 to 0.2.0

The MSI is a major upgrade. It replaces the binaries and **keeps**
`%ProgramData%\SoftafriqueBackupAgent\credentials.dat`, so a patch never leaves a
working device unenrolled.

The catch is that 0.1.0 was never enrolled: it had no credentials blob, because
it had no way to talk to the gateway. So 0.1.0 to 0.2.0 is an *enrollment*, not
an upgrade. Run `install.ps1` with a token. The old `repo` line and
`password_file` in `config.yaml` are replaced; the plaintext password file is
deleted.

Verify after the upgrade:

```powershell
.\scripts\rmm\health-check.ps1
Get-Content "$env:ProgramData\SoftafriqueBackupAgent\status.json" -Raw
```

There should be no `repo` field, and no `password_file` pointing inside the data
directory.

## Signing

The pilot ships unsigned. Before GA, on a machine with the Softafrique code
signing certificate:

```powershell
signtool sign /fd SHA256 /tr http://timestamp.digicert.com /td SHA256 `
  /f softafrique-code-signing.pfx /p $env:CERT_PASSWORD `
  "SoftafriqueBackupAgent.msi" `
  "SoftafriqueBackupAgent.exe" "restic.exe" "validatepath.exe"
signtool verify /pa /v SoftafriqueBackupAgent.msi
```

Sign the MSI and all three exes. An MSI whose payload is unsigned will install on
a pilot machine and fail on anything with SmartScreen or AppLocker enforced,
which is most managed estates. Re-test the RMM deployment after signing, because
Tactical RMM's own download step verifies the hash it was given.

## Manual install (no RMM)

```powershell
msiexec /i SoftafriqueBackupAgent.msi /qn BACKUPPATH="D:\CustomerData"
$env:SOFTAFRIQUE_ENROLL_TOKEN = "<one-time token>"
& "C:\Program Files\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" enroll
Remove-Item Env:\SOFTAFRIQUE_ENROLL_TOKEN
Start-Service SoftafriqueBackupAgent
```

## What is not automated yet

* **Retention.** The gateway is append-only and the agent has no `forget` or
  `prune` code path. Until a retention job exists, storage grows without bound.
  That is the safe default, not an oversight, but it needs a server-side answer
  before the fleet gets large.
* **Key escrow retrieval.** The dashboard holds the escrowed key. Nothing in the
  agent fetches it, by design; `agent reimport` takes it as a file from an
  operator.
* **Certificate-based TLS pinning.** Go enforces TLS 1.2 minimum. There is no
  pin, so a machine trusting a rogue CA could be intercepted. Deferred until
  mohsin decides whether the gateway will publish a stable certificate chain.
* **Windows test coverage on CI.** DPAPI, the named mutex and the service
  wrapper are vetted with `GOOS=windows` and tested natively on
  `windows-latest`, but they are not tested on a real domain-joined Server 2019.
  Do that before GA.
