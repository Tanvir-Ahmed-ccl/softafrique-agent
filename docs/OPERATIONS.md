# Operations runbook

Everything an on-call engineer or a support technician needs. Written for the
0.2.x line, currently 0.2.36.

## Where things are

| Path | Contents |
| --- | --- |
| `C:\Program Files\Softafrique Backup Agent\` | `SoftafriqueBackupAgent.exe`, `restic.exe`, `validatepath.exe` |
| `C:\ProgramData\SoftafriqueBackupAgent\` | `config.yaml`, `status.json`, `agent.log`, `credentials.dat`, `remote.json` |
| `HKLM\SOFTWARE\Softafrique\BackupAgent` | InstallDir, DataDir |
| Service | `SoftafriqueBackupAgent`, auto + delayed, running as LocalSystem |
| Starts at | Install, by `StartServices`. A token-file install is enrolled before that runs. Without a token the service stays running and waits for enrollment; exiting during `StartServices` would fail the install with error 1920. The RMM script enrols after the MSI and restarts the service afterwards. |

`credentials.dat` is the only file with anything sensitive in it, and it is
DPAPI-sealed in machine scope. Sealing is not the protection: anything running as
SYSTEM or an administrator can decrypt a machine-scope blob, so the directory ACL
is the boundary. The package grants full control to SYSTEM and Administrators,
inheritable, with inheritance switched off, and nothing else. On an upgrade it
also resets existing children to inherit that DACL, because Windows does not
retroactively re-propagate a changed parent ACL to files that already exist.

The practical consequence is that reading `status.json` needs elevation. The
Tactical RMM health check runs as SYSTEM and is unaffected. A third-party
monitor running as an ordinary user cannot open it, and must be given elevation
or have an administrator copy the file out. An earlier draft of the package
granted `Users` read on the directory to solve this; it did not work, because a
non-inheritable directory ACE does not reach the files inside it.

## Uninstalling

A standalone MSI uninstall removes the service, the sealed credentials and the
agent's local state: `status.json`, `remote.json`, `config.yaml`, `agent.log` and
`restic-cache`. It leaves the customer's protected folder and the server-side
repository untouched, so history survives an uninstall and reinstall.

Before deleting `credentials.dat`, the uninstall calls `POST /withdraw` with the
device's own credentials. That disables the device at the gateway and removes it
from the repository's credential file, so restic calls start returning 401 at
once. The repository, the escrowed key and every snapshot are retained. The call
is idempotent and best effort: a machine with no network still uninstalls, with
the failure recorded in `agent.log` and the RMM job output. If that happened,
withdraw the device from the dashboard. To bring a withdrawn device back, an
administrator must call the gateway's
`/api/v1/admin/device/{device_id}/reenroll`; it is not something the agent can do
to itself.

`agent unenroll` does **not** withdraw: it is the first half of the documented
re-enrollment path. Use `agent withdraw` explicitly if a device is being
decommissioned without the MSI.

`uninstall.ps1` captures the last status line in its job output first, then
performs the same cleanup. Use `-KeepStatus` if a copy of `status.json` is needed
for a ticket; it is written to `%TEMP%`, not back into the data directory. The
old `-PurgeCredentials` switch is accepted for compatibility but the data
directory is now removed by default.

If only the credentials are removed by hand, `status -short` reports
`enrolled=false` immediately. It checks for `credentials.dat` rather than
trusting the last status file.

## Reading the status file

```powershell
Get-Content "$env:ProgramData\SoftafriqueBackupAgent\status.json" -Raw | ConvertFrom-Json
```

| Field | Read it as |
| --- | --- |
| `enrolled` | `false` means the device has no key. Nothing is being protected. `status -short` reads this from the credentials blob, not the stale file, so a leftover `status.json` cannot claim a removed device is still enrolled. |
| `server_status` | The gateway's view. Anything but `active` means backups are withheld. |
| `action_required` | Written for a human. If it is set, read it; it says what to do. |
| `last_attempt_status` | `running`, `success`, `failed`, `suspended` or `recreated`. `suspended` is not a failure: nothing was attempted. `recreated` means the protected folder was missing and was recreated, but no snapshot was taken: `last_success` did not move, `action_required` stays set, and the fix is a site visit. |
| `last_attempt_start` / `last_attempt_end` | Bracket the last attempt, so a `running` older than a few hours is a stuck restic. |
| `last_success` | When data was last actually protected. This is the field that matters. |
| `last_duration_seconds` | Wall clock, including waiting for the network. This is what to quote a customer. |
| `restic_duration_seconds` | restic's own number. Never quote this as the backup duration. |
| `consecutive_failures` | Drives the "failing" alert. |
| `repo_stats.restore_bytes` / `repo_stats.file_count` | Restore size and restic's `total_file_count` for the **latest snapshot**. restic counts directories as nodes, so `file_count` is items (files plus directories plus the snapshot root), not a pure file count. The dashboard should label it "items", not "files" or "files protected". |
| `repo_stats.repo_bytes` | Raw space the whole repository occupies, including history. |
| `repo_stats.captured_at` | When the usage numbers were read, so a stale figure is distinguishable. |
| `schedule_interval_seconds` | The effective interval after any gateway override. Use it for staleness thresholds. |
| `next_backup_time` | When the agent plans to run next. It is empty immediately after enrollment because no attempt has set a plan yet; the first scheduled run (or a failed attempt, which plans a retry) fills it. An empty value at enrollment is not a fault. |

`last_backup_status` and `last_backup_time` are the 0.1.0 names for
`last_attempt_status` and `last_success`. They are kept so monitoring written
against 0.1.0 keeps working, and will be dropped in a future major version.

A 0.1.0 `status.json` is migrated on read: the `repo` field, which held the
repository URL with the password embedded in it, is dropped and the file is
rewritten as schema 2. The 0.1.0 `password_file` is deleted on upgrade if it
lives inside the data directory.

## Changing the protected folder after install

Use the dashboard. The gateway's `backup_path` overrides the local `include` on
an enrolled device, so editing `config.yaml` on the machine is not the supported
way to change what is protected.

The MSI's **Change** is disabled in maintenance mode because the package has no
independently selectable features. That is deliberate: reinstalling just to
alter a path is more error-prone than making the change where the fleet is
managed, and an install-time change would be overwritten by the gateway anyway.
To move a device to a different folder, change `backup_path` for that device in
the dashboard and wait for the next `/config` poll (or restart the service to
fetch it immediately).

The one exception is a device that must override the gateway, which support does
by editing `include` in `config.yaml` and restarting the service. It is not a
routine change and it makes the local file differ from the dashboard, so record
why it was done.

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
& "C:\Program Files\Softafrique Backup Agent\restic.exe" snapshots
```

Do not add `--host`: it filters the list, and snapshots written before the
device-id host was introduced carry the machine hostname instead. The agent's
own `snapshots` command intentionally lists the whole repository for the same
reason.

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

### "It says the protected folder was missing"

`last_attempt_status` is `recreated`. Someone deleted the folder the device was
asked to protect, and the agent recreated it rather than failing every hour
forever. The run that followed deliberately took **no snapshot**, because an
empty directory protects nothing; the device is running but not protected.

This needs a site visit: confirm the customer's data is actually in the path named
in `action_required`. If it is not, they moved or lost it, and the snapshots
before the deletion are the only copy.

`last_success` deliberately did not move, so the staleness check above will also
fire once the threshold passes. That is intended: it is the same condition seen
from the other direction. The notice clears itself on the next run that backs up
real data.

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

This is worth being precise about, because the gateway guarantees that an
unenroll-then-re-enroll returns the *same escrowed key*, and it is easy to
misread that as "0.2.0 upgrades need no token". It does not. That guarantee covers
a device the gateway already knows, and 0.1.0 is not one: the gateway holds no
key for it, so its first enrollment mints one. There is no history at risk,
because there is no gateway-side history for that device.

A `config.yaml` still carrying the retired `auto_init` key loads and ignores it.
This is deliberate: the MSI writes `config.yaml` with `NeverOverwrite`, so a
stale file from 0.1.0 is *not* replaced on a major upgrade, and refusing to start
over a key that no longer does anything would turn a cosmetic leftover into an
outage. Genuinely unknown keys are still refused.

The full procedure, including the re-enrollment case that does **not** need a new
key, is in [`JOINT-SESSION.md`](JOINT-SESSION.md).

Verify after the upgrade:

```powershell
.\scripts\rmm\health-check.ps1
Get-Content "$env:ProgramData\SoftafriqueBackupAgent\status.json" -Raw
```

There should be no `repo` field, and no `password_file` pointing inside the data
directory.

## Signing

The current pilot ships unsigned while the certificate secrets are absent. Before GA, configure the code signing
certificate in CI with these repository secrets:

* `SOFTAFRIQUE_SIGNING_PFX_BASE64`: the PFX file, base64-encoded.
* `SOFTAFRIQUE_SIGNING_PFX_PASSWORD`: the PFX password.

On a tag build, `scripts/sign-windows.ps1` signs
`SoftafriqueBackupAgent.exe` and `validatepath.exe` before the MSI is built,
then signs the MSI and runs `signtool verify /pa` on each file. The install
smoke test runs after signing, so the signed package is installed as well as
verified. While the secrets are absent the step prints a skip and the build
remains unsigned. If you publish a signed MSI, compute its SHA256 **after**
signing; the signature changes the hash.

The script does **not** re-sign `restic.exe`. That is third-party code, and
signing it with Softafrique's certificate would assert authorship of a binary
Softafrique did not build.

An MSI whose payload is unsigned will install on a pilot machine and fail on
anything with SmartScreen or AppLocker enforced, which is most managed estates.
Re-test the RMM deployment after signing, because Tactical RMM's own download
step verifies the hash it was given.

## Manual install (no RMM)

An interactive install shows a **Folder to protect** page after the license
page, pre-filled with `C:\SoftafriqueBackup`, so a hand-typed `msiexec /i` does
not silently protect an empty default folder. There is no install-location page
in the wizard: the service binary has no reason to live anywhere but Program
Files, and two similar folder prompts in a row caused a technician to install
the service into `C:\Windows`. The silent form sets the same value on the
command line:

```powershell
msiexec /i SoftafriqueBackupAgent.msi /qn BACKUPPATH="D:\CustomerData"
$env:SOFTAFRIQUE_ENROLL_TOKEN = "<one-time token>"
& "C:\Program Files\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" enroll
Remove-Item Env:\SOFTAFRIQUE_ENROLL_TOKEN
Start-Service SoftafriqueBackupAgent
```

Both forms run the same `validatepath.exe` check in the execute sequence. It is
deferred, so on the interactive path a refused folder ends the install with a
rollback and the reason in the MSI log; run `validatepath.exe` by hand first to
see the sentence before committing to the path.

The check runs **before** `config.yaml` is written. A refused install therefore
leaves no config behind, and a rollback custom action removes the config if a
later step (enrollment, the ACL reset, the service start) fails after it was
written. This matters because `render-config` preserves an existing
`config.yaml` on repair and upgrade: a config left by a failed attempt used to be
treated as an on-site edit and kept, so the next install could succeed while
protecting the folder the previous attempt had been refused. As a further repair
path, `render-config` replaces an existing config whose `include` names a
protected system location such as `C:\Windows`, rather than preserving a folder
the agent will never back up.

A failed upgrade must also leave the working install alone. `RemoveExistingProducts`
is therefore scheduled with `afterInstallFinalize`: the new package has to
succeed before the old one is removed. The default schedule uninstalls the old
product before the new one is validated, so a mistyped upgrade path would take a
protected customer offline instead of simply failing the upgrade.

A directory property can end in a backslash, and a quoted command-line argument
that ends in `\` escapes its own closing quote. The MSI therefore passes the
folder as `<path>\.` to both the validator and `render-config`, and the Go side
normalises that sentinel once with `pathpolicy.NormalizeDir`. An existing
`config.yaml` that carries the old escaped value is repaired in place at the
next install, without discarding other on-site edits. The CI install job runs a
real install with a trailing separator on `BACKUPPATH`, asserts the service is
running, checks the rendered `include` value, and then uninstalls. That is the
check a compiled-table assertion cannot provide.

## What still needs a human or a server-side decision

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
* **A human walk-through of the installer.** CI installs, repairs and uninstalls
  the MSI, asserts the compiled UI tables, and proves the folder checks hold, but
  a person still has to walk the wizard on a real Windows host before release;
  see [`JOINT-SESSION.md`](JOINT-SESSION.md). The RMM path does not use the UI:
  `install.ps1` prompts for `-BackupPath` in PowerShell and installs with `/qn`.
* **The fixed-disk check, on a real machine.** The policy is confirmed: fixed
  disks only, a network share refused unless somebody opted in, and optical or
  removable drives refused with no opt-in. It lives in `internal/pathpolicy` and
  is applied twice — by `validatepath.exe` on what a technician typed, and by the
  agent on every path before restic is handed it — so the installer and the
  running agent cannot disagree.

  It uses `GetDriveType`, so the volume half exists only in the Windows build;
  off Windows `internal/pathpolicy` applies the share half and passes the volume
  half through, which is stated in `volume_other.go` rather than hidden. The
  assertions are in `volume_windows_test.go` and run on the `windows-latest` CI
  job. **None of that is a real Server 2019 with a real DVD drive**, which is the
  machine this bug came from. Do that before GA: see
  [`JOINT-SESSION.md`](JOINT-SESSION.md).
* **Windows test coverage on CI.** DPAPI, the named mutex, the service wrapper
  and the volume-type classifier are vetted with `GOOS=windows` and tested
  natively on `windows-latest`, but they are not tested on a real domain-joined
  Server 2019. Do that before GA: see [`JOINT-SESSION.md`](JOINT-SESSION.md).
* **Creating an SMB share from the installer.** Not built, and nothing depends on
  it. A share has to exist and be shared before the install, and the machine
  account has to have been granted access to it — the opt-in covers the sites that
  need one. See
  [`gateway-contract.md`](gateway-contract.md#5-fixed-disk-policy-for-the-protected-folder).
