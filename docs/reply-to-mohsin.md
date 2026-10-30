# Reply to Mohsin — agent build, findings A, D and E

For the joint document. Joins the gateway-side reply; where the two touch, this is
the agent's position.

---

## A. Restic repository isolation

**Finding.** 0.1.0 built the repository URL from a `device_id` that defaulted to
the Windows machine name, under `/repos/{device_id}`. Two machines with the same
name therefore addressed the same repository, and a rename silently started a new
one. The URL could also carry the password, which put the key in the config file
and in `status.json`.

**Fixed in 0.2.0.**

- The repository is now whatever `POST /enroll` returns, and nothing else. On an
  enrolled device a stale `repo:` line in `config.yaml` is ignored, so an old file
  cannot point a paying customer at the wrong place.
- `restic --host` is the enrollment-assigned `device_id`, never the machine name.
  It is the same value the gateway knows the device by, which is what makes
  central reporting attributable.
- The agent refuses a `device_id` that is not a plain identifier (anything with
  path separators, whitespace or unusual characters). It does not sanitise it: a
  surprising id from the gateway is a gateway bug to fix, not something to paper
  over, because sanitising would change which repository is addressed.
- The repository URL must not contain credentials. `secret.Credentials.Validate`
  rejects one, and the check is a test, not a comment.
- Repositories are served at the root, `rest:https://backup.softafrique.net/{device_id}`,
  matching what the gateway serves. The old `/repos/` prefix is gone.
- The agent has **no `restic init` code path at all**. The gateway creates the
  repository server-side at enrollment and fails the enrollment if its own init
  fails, so a missing repository is terminal and is reported as such in
  `status.json` rather than being papered over with a new empty one at whatever
  path a wrong device id happens to name. The `auto_init` key is gone rather than
  defaulted off, because an option that can be set is an option somebody will set.
  A `config.yaml` still carrying the old key loads and ignores it, so upgrading
  a 0.1.0 device does not stop the service dead. `TestNoInitCodePathExists` reads
  this package's own source and fails if an init invocation comes back.

**Answered — no longer open.** You confirmed `/enroll` creates the repository and
fails if initialization fails, so nothing is needed from you here.

---

## D. Credential and key handling

**Finding.** 0.1.0 took the repository password as a command-line argument, wrote
a second copy into `status.json`, and derived the API username from the machine
name. The process table, the status file and any screenshot of either exposed the
key that decrypts a customer's entire history.

**Fixed in 0.2.0.**

| Secret | Where it lives | How it reaches restic |
| --- | --- | --- |
| `encryption_key` (repository password) | `credentials.dat`, DPAPI machine scope | `RESTIC_PASSWORD` in the child process environment |
| `device_password` (HTTP Basic) | same blob | HTTP Basic header |

- The key is never in a command line, never in `config.yaml`, never in
  `status.json`, never in the log, and never in the MSI. The enrollment token
  reaches the agent in an environment variable or a file the agent deletes once
  consumed, so an MSI property or a command line is not needed for it either.
- DPAPI is `CRYPTPROTECT_LOCAL_MACHINE | CRYPTPROTECT_UI_FORBIDDEN`, because the
  service runs as LocalSystem and has no user profile to scope to. Be clear about
  what that does and does not buy: it stops other *users*; anything running as
  SYSTEM or an administrator can still decrypt. The real control is the
  directory ACL, which the MSI sets explicitly on
  `%ProgramData%\SoftafriqueBackupAgent` to SYSTEM + Administrators full control,
  with inheritance disabled, and no `Users` read (an earlier package granted one
  and it did not work, because a non-inheritable directory ACE does not reach the
  files inside), so a later widening of `%ProgramData%` cannot silently expose it.
- The 0.1.0 `password_file` is deleted during upgrade when it lives inside the
  data directory. A file the operator put elsewhere is left alone.
- `status.json` contains no credential field by construction: no password, no
  repository URL, only `repo_host`. A test reads the file after a run and fails if
  any known secret value appears in it.
- The two secrets are never compared or used interchangeably. The 0.1.0 design
  had no separation at all, which meant one leak would have handed over both the
  API and the data.

**Asked, and now answered.** These four were open when this was written. Your
replies close all of them, and the agent is coded to what you said:

1. **`/enroll` on an existing device.** Confirmed: the device is never
   re-enrolled, and `/enroll` returns `409` for a device that is already
   enrolled. The only route to a new enrollment is unenroll first, and then
   `/enroll` returns the **same escrowed key**. So `encryption_key` is stable for
   the life of a device, and an `agent enroll` against an enrolled device is a
   no-op that reports the existing device rather than a second one.
2. **Re-enrollment and 409.** Confirmed, and `agent enroll` now says so in those
   words instead of retrying a request that will never succeed.
3. **The `rest:` prefix.** Confirmed: `repository` is returned complete, exactly
   `rest:https://backup.softafrique.net/{device_id}` — no `rest:` prefixing by
   us, no `/repos/` path element, no userinfo, and no trailing slash. We still
   reject a URL that arrives with credentials in it, because a correct reply
   should never contain one and a wrong one should be noticed.
4. **Repository usage in `/status`.** Confirmed: it does not belong there, and
   `/status` does not take it. We read `restic stats` locally and keep it in
   `status.json` for the dashboard and for Tactical RMM, and we send `/status`
   exactly the nine declared fields. Nothing undeclared is added to your schema.

**One consequence worth naming, and one place it does not apply.** Because
unenroll-then-enroll returns the same key, a device that *was* enrolled can be
re-enrolled without a new key ever being minted — so a re-enrollment can no
longer orphan a customer's existing snapshots. That was the failure mode we were
worried about, and your answer closes it.

It does **not** apply to a 0.1.0 device, and it would be easy to misread your
answer as saying otherwise. 0.1.0 never talked to the gateway, so you hold no
escrowed key for it and its first 0.2.0 enrollment mints one. There is no
gateway-side history at risk, because there is no gateway-side history. A
0.1.0 → 0.2.0 upgrade is therefore a first enrollment and does need a token;
`docs/OPERATIONS.md` and `docs/JOINT-SESSION.md` both carry the procedure.

---

## E. Status reporting: what a customer is told

**Finding.** 0.1.0 reported success before a backup had run, wrote a run that had
never happened as a success, and reported restic's own duration as the backup
duration, so a backup that waited three and a half minutes for the network and
then worked for one second was logged as one second.

**Fixed in 0.2.0.** The `/status` body is exactly the nine fields in the
contract, nothing added, because we are not going to guess at a schema. The richer
view lives in `status.json` for Tactical RMM:

- `last_attempt_status` (`running`, `success`, `failed`, `suspended`, `recreated`) and
  `last_attempt_start` / `last_attempt_end` are separate from `last_success`, so
  "when was this device last actually protected" is answerable even after a week
  of failures.
- `last_duration_seconds` is wall clock, measured by the agent, and is the number
  to quote a customer. `restic_duration_seconds` is restic's own and is labelled
  as such.
- A run that never happened is not a failure. A suspended or unenrolled device
  reports `suspended` with no error, and the agent does not tell the gateway a
  backup failed when it never tried.
- A `status` value that is not `active` stops backups, including one we do not
  recognise. Defaulting "unknown means carry on" would let a decommissioned
  device keep writing to a repository.
- `action_required` is written for a human, e.g. `re-enroll: the gateway no
  longer accepts this device's credentials`. It is cleared only by a
  `/config` reply that says active, never by a successful status POST, which
  would paper over a suspension.

**Behaviour you should know about, because it is deliberate.**

1. A `GET /config` that fails on a network error or a 5xx **does not stop
   backups**. The last known configuration is kept and the run proceeds. Only a
   successful reply saying the device is not active withholds backups. If you
   would rather we failed closed on any `/config` error, say so — it is a policy
   choice, not a technical limit, and we will change it.
2. A 401 or 403 from `/config` or `/status` is terminal: the agent stops retrying
   and asks for a re-enrollment. Retrying a rejected credential every five minutes
   against a server that will keep saying no is noise.
3. `last_backup_status` is only ever `success` or `failed`, and is sent after an
   attempt has finished, never while one is running.

---

## F. Which folder this agent will protect

**Confirmed policy, agreed with you:** a fixed disk only. A network share is
refused unless a person opts in, and an optical or removable drive is refused
outright.

**Implemented in one rule, applied twice.** `internal/pathpolicy` holds the
decision, and both entry points call it: the MSI's `validatepath.exe` on the
`BACKUPPATH` a technician typed, and the agent on every path before restic is
handed it. It is one package rather than two checks because the two happen a year
apart — the installer runs once, with a human present, and the agent runs hourly
with nobody watching — and a second copy of a policy is a policy that eventually
disagrees with itself. `decide` is separated from the volume lookup for the same
reason: the tests can assert the whole table of volume types without depending on
which drives the machine running them happens to have.

The run-time half is not redundant. It catches what the *gateway* says — a
`backup_path` on `/config`, or an enrollment reply — and a share or a DVD drive
letter is exactly the kind of value a dashboard can be made to point somewhere by
mistake, by an import, or by a support engineer guessing. It refuses **before**
restic is given the path, because that is the last point at which the agent is
still the one deciding. It is terminal rather than retried, because retrying every
five minutes against a share the machine account cannot read produces the same
failure forever and buries it.

**The refusal is written for the person who has to fix it.** It names the paths,
says what volume each one is on, and gives the fix in the vocabulary the reader
has — `-allow-unc` in an installer log, `allow_unc: true` in a status file. When a
share is involved it also says why, because "refused by policy" at three in the
morning is not actionable and the reason is genuinely not obvious: the service
runs as LocalSystem, so a share that opened perfectly during installation as the
technician can still fail every hourly backup because the *machine account* was
never granted access.

**Where the opt-in is recorded.** `install.ps1 -AllowUNC` passes `ALLOWUNC=1` to
the MSI, so the installer's own check agrees, and then writes `allow_unc: true`
into `config.yaml` after the install. The MSI's property is gone by then and the
agent still has to know, and it is worth being explicit about why that is not
a second source of truth: the switch decides both halves, and the config key is
where the agent's half is stored. We did not have the MSI substitute the key into
the config template, because a `ConfigurableTextFile` placeholder that produces
`allow_unc: [false]` is a config file the agent cannot parse, and we have no way
to test that substitution without a Windows Installer toolchain. Better to
write it where a person can read it.

**What the agent will not do.** It does not create an SMB share. A share has to
exist and be shared before the install, and the machine account has to have been
granted access to it. Whether the MSI should offer to do that is the one open
item in this section; it is not built, and nothing depends on it.

CI runs `go test`, `go vet` for `GOOS=windows`, a native Windows test run, and an
MSI install/uninstall smoke test on `windows-latest`.

---

## Evidence

Each fix above has a test named after the failure it prevents, so a regression
fails the build rather than waiting to be noticed on a customer machine:

| Test | What it pins |
| --- | --- |
| `TestArgsNeverContainRetentionCommands` | the agent has no `forget`/`prune` code path |
| `TestArgsCarryHostAndNoPassword` | the key is not on a command line, and `--host` is the device id |
| `TestStatusFileNeverContainsSecrets` | no secret value appears in `status.json` |
| `TestNoCredentialFieldExists`, `TestMarshalledStatusContainsNoSecret` | the schema cannot hold a credential, by construction |
| `TestUnenrolledDeviceIsTerminal` | an unenrolled device does not retry forever |
| `TestSuspendedDeviceDoesNotBackUpAndIsNotRetried` | a suspended device writes nothing |
| `TestUnknownGatewayStatusSuspends` | an unrecognised state is not permission |
| `TestGatewayOutageStillBacksUp` | a `/config` failure does not stop the backup |
| `TestRejectedCredentialsSuspendAndAreActionable` | a 401 is terminal and says what to do |
| `TestMissingRepositoryIsTerminal` | no `init` after a transient failure |
| `TestNoInitCodePathExists` | there is no init invocation anywhere in `internal/restic`; it reads the package's own source |
| `TestAttemptRecordsElapsedNotResticTime` | elapsed is wall clock, not restic's number |
| `TestFailedBackupKeepsLastSuccess` | a failure does not move last success |
| `TestImplausibleDeviceIDIsRejected` | a hostile device id cannot redirect the repository |
| `TestMigrationRemovesTheLegacyPasswordFile` | the 0.1.0 plaintext key is deleted |
| `TestANetworkShareIsRefusedBeforeResticIsGivenThePath` | a share never reaches restic without the opt-in |
| `TestIsUncTellsSharesFromOrdinaryPaths` | `C:\` is not read as a share; the customer's own drive is not refused |
| `TestOnlyAFixedDiskIsAllowed` (Windows) | a DVD drive is refused whatever the machine has mounted |
| `TestTheShareOptInDoesNotAllowOtherVolumeTypes` (Windows) | `-allow-unc` accepts a share and nothing else |
| `TestDeletedProtectedFolderIsRecreatedAndNotAFailure` | a folder deleted after install is recreated and reported as `recreated`, not as a green backup of an empty directory |
| `TestTheOptInIsOffInEveryShippedConfig` | no shipped config can be installed already allowing a share |

CI runs `go test`, `go vet` for `GOOS=windows`, a native Windows test run, and an
MSI install/uninstall smoke test on `windows-latest`. Two of the tables above are
Windows-only tests and are marked as such: `internal/pathpolicy`'s volume
assertions cannot be exercised off Windows, because there is no drive table to
read, and a check that can only be run on the machine that ships is a check that
would otherwise never be run at all.

---

# Reply to the gateway hand-off — 9 October

The four items you asked for, the agent-side findings log, and the corrections
that came out of writing this. Where a claim is provable from source or CI it
says so; where it needs the Windows session the command and expected value are in
section 5, with the result left blank for the run.

## 1. MSI signing — recommendation: sign before any deployment

**Sign the pilot, not just GA.** This goes to Kwame as the recommendation, with
the reasoning attached.

An unsigned MSI is not a "Windows will warn" inconvenience. On any estate with
SmartScreen or AppLocker enforced it is **blocked outright**, which is most
managed estates, and it fails at the least useful possible moment — the technician
has committed to the deployment and the failure looks like the agent is broken
rather than like the package is untrusted. A failed site deployment costs a
technician visit, a support call and confidence; a code-signing certificate is a
predictable annual line item. The pilot sites are real customer sites, so the
pilot is already in the population the block applies to.

Mechanically, sign the MSI **and all three exes** (`SoftafriqueBackupAgent.exe`,
`restic.exe`, `validatepath.exe`) with SHA-256 and a timestamp; an MSI whose
payload is unsigned still trips the checks, and the procedure is already written
in `OPERATIONS.md` under **Signing**. Two certificate options:

- **OV (organisation-validated):** cheaper; SmartScreen reputation builds over
  downloads, so early sites may still warn.
- **EV (extended-validation):** higher annual cost; immediate SmartScreen
  reputation, no warm-up.

**Recommendation:** EV if any pilot site enforces SmartScreen or AppLocker, which
you have said some do. If the budget only stretches to OV, sign anyway — OV is
strictly better than unsigned — and expect warnings until reputation builds.
Either way, re-test the RMM deployment after signing, because Tactical RMM
verifies the hash it was handed and a re-signed artefact changes it.

## 2. Deliverable sections — the agent half

### Agent architecture, and why Go

The agent is one statically linked Windows service binary plus a bundled copy of
`restic.exe`, cross-compiled from a single source tree and shipped with no runtime
dependencies — no VC++ redistributable, no installed interpreter, nothing the
customer's server has to already have. Go was chosen for exactly that: one
self-contained binary that drops onto a Server 2019 box and runs, full stop. It
also gives first-class access to the Windows APIs the security design depends on
— machine-scope DPAPI (`CryptProtectData`), the Service Control Manager, a global
named mutex, and `GetDriveType` — through `golang.org/x/sys/windows`, with the
standard library covering HTTP/TLS, which is all the gateway client needs. The
practical payoff is a build we can trust from CI: `GOOS=windows go vet` and a
native `windows-latest` test run on every change, so the Windows-only paths are
compiled and exercised rather than discovered broken on a customer machine. The
code is split so each decision has one home: `internal/agent` (orchestration),
`internal/api` (gateway client), `internal/config`, `internal/secret` (DPAPI),
`internal/restic`, `internal/scheduler`, `internal/service`, `internal/status`,
and `internal/pathpolicy` — the last being one folder-policy rule that both the
installer and the running agent call, so they cannot disagree.

### Installer approach, and why

One WiX v5 MSI, per-machine, a major upgrade. An MSI is the native Windows
deployment unit, so it is what our RMM already knows how to push, what Group
Policy can push, and what gives us rollback and clean uninstall for free. It lets
the folder validation run as a deferred SYSTEM custom action *during* the install,
so a path that will not work fails the install instead of failing every hourly
backup afterwards — a technician sees the error while they are still at the
machine. That validation is `validatepath.exe`, which calls the same
`internal/pathpolicy` the running agent uses. `config.yaml` is rendered by the
agent's own `render-config` rather than by MSI text substitution, because WiX 4
removed `ConfigurableTextFile` and a bad substitution produces an unparseable
config; doing it in the agent means the file is parsed and validated before it is
written, and an on-site edit survives a repair. The installer never creates an SMB
share and never creates a repository — both are deliberate, and section 3 covers
the share.

### Scheduling and offline handling

There is no Task Scheduler entry. The service is its own scheduler, which keeps
the schedule, the credentials and the status in one place and means schedule
state survives a reboot. The next run time is `last_success + schedule_interval`,
read from `status.json`, so a machine that was off comes back and simply runs; the
catch-up is a normal incremental check. Failed runs retry with equal jitter, and
by default retry forever, because a laptop that was unplugged needs to catch up
rather than give up. When the device is enrolled, the gateway's
`schedule_interval` and `retry_interval` override the local values.

Offline behaviour is the part with a policy in it. A `GET /config` that fails on a
**transport error or a 5xx keeps the last known configuration and the backup
proceeds** — data protection must not depend on the control plane being reachable.
`remote.json` is the cache, so a device that boots with no network still knows its
folder and schedule. The agent fails closed only on a deliberate signal from the
gateway: a *successful* reply whose `status` is not exactly `active`, or a
`401`/`403`, which means the credentials are dead. We are keeping this behaviour,
per section 6.

### Credential storage

Two secrets, kept separate. `device_password` is HTTP Basic for the API;
`encryption_key` is the restic repository password. Both are returned by
`/enroll` and written to `credentials.dat`, sealed with DPAPI in **machine
scope** (the service runs as LocalSystem and has no user profile to scope to).
Neither secret is ever in `config.yaml`, `status.json`, `agent.log`, the MSI, or a
command line; the key reaches restic only as `RESTIC_PASSWORD` in the child
process environment. Be precise about what DPAPI buys: it stops other *users*, not
anything running as SYSTEM or an administrator, so the **real** control is the
directory ACL the installer sets on `%ProgramData%\SoftafriqueBackupAgent` —
SYSTEM and Administrators full control, inheritance disabled, and nothing else.
`status.json` stores `repo_host`, never the full URL. Tests read the file after a
run and fail if any known secret value appears in it. The 0.1.0 plaintext
`password_file` is deleted on upgrade when it lives inside the data directory.

### Upgrade mechanism

The MSI is a major upgrade (same `UpgradeCode`, same-version upgrades allowed). It
replaces the binaries and deliberately **keeps** `credentials.dat`, so a patch can
never leave a working device unenrolled; `config.yaml` is `NeverOverwrite` so an
edit made on site survives a repair or upgrade. There is no in-agent self-update:
updates come down the same trusted MSI/RMM path as the original install, so the
agent never downloads and executes code on its own, which keeps the attack surface
on the data side as small as possible. Note the one case that is an *enrollment*
rather than an upgrade: 0.1.0 never talked to the gateway and has no credentials
blob, so 0.1.0 → 0.2.0 is a first enrollment and needs a token. Documented in
`OPERATIONS.md`.

### RMM integration

Tactical RMM is the target. Three scripts live in `scripts/rmm/`:

- `install.ps1` — validates the folder **before anything touches the network**,
  installs the MSI with `BACKUPPATH`, adds `ALLOWUNC=1` only when `-AllowUNC` was
  passed, enrolls with the token from `SOFTAFRIQUE_ENROLL_TOKEN`, starts the
  service, waits for `status.json` to say `enrolled = true`, and exits non-zero
  with the MSI log tail on failure.
- `health-check.ps1` — reads `status.json` and exits `0` healthy, `1` unhealthy,
  `2` not enrolled, `3` nothing to check yet. It fails when a machine *stops being
  protected*, not only when the service stops: suspended, `action_required`, a
  `recreated` folder, a UNC path, or a stale `last_success`. It runs as SYSTEM.
- `uninstall.ps1` — removes the service and files; snapshots and the escrow key
  are untouched.

The machine-readable contract is `status.json` (no secrets). The agent CLI also
carries monitor-friendly exit codes (`0` ok, `3` not enrolled, `4` action
required) for one-off RMM checks. One point that matters to you directly:
dashboard liveness keys off **`POST /status`**, which fires after every attempt
and at least every `status_heartbeat` while idle — which is exactly the signal
your missing alerting would hang off.

## 3. The SMB share question — agreed

Your position is our position, and the current build already implements it: the
opt-in is **off by default**, the administrator chooses the permissions, and the
agent never creates a share. `install.ps1 -AllowUNC` (or `allow_unc: true` in the
config a person edits) is the only way a share is accepted, and even then it has
to already exist, already be shared, and already grant the machine account access.
We deliberately do **not** build share creation into the installer — it drags in
share permissions, NTFS permissions and the machine account for a minority of
sites, and every one of those is a new way to expose a customer's data on their
own network. Nothing depends on it. The reasoning is written up in
`gateway-contract.md` §5 and `OPERATIONS.md`.

## 4. Manual install for 0.2.0 (no MSI)

For running the real 0.2.0 without the MSI. Order is **files + config → install
service → enroll → start**; `agent backup` is only a manual one-off and is not
part of the install.

**Files beside the agent** in `C:\Program Files\Softafrique Backup Agent\`:

- `SoftafriqueBackupAgent.exe` — required.
- `restic.exe` — required. `restic_path: "restic"` resolves to the copy beside
  the agent, so the customer machine needs nothing else installed.
- `validatepath.exe` — optional; the MSI uses it for the install-time folder
  check, and it is handy by hand.
- `config.yaml.template` — put it in the data directory, which is where the MSI
  puts it, so the step below reads it from the same place on every machine.

**1. Create the data directory and render `config.yaml`.** `render-config` writes
its output into the data directory and does not create that directory, so create
it first. It also refuses to overwrite an existing `config.yaml`: on a device
that already has one it prints `kept existing ...` and changes nothing, which is
deliberate — it is what preserves on-site edits across a repair or an upgrade.

```powershell
$d = "$env:ProgramData\SoftafriqueBackupAgent"
New-Item -ItemType Directory -Force -Path $d | Out-Null
Copy-Item "C:\Program Files\Softafrique Backup Agent\config.yaml.template" $d

& "C:\Program Files\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" render-config `
  -template "$d\config.yaml.template" `
  -out "$d\config.yaml" `
  -backup-path "D:\CustomerData" `
  -data-dir $d `
  -status-file "$d\status.json" `
  -log-file "$d\agent.log"
```

**2. Lock down the data directory** — this ACL, not DPAPI, is the real protection
for the sealed key:

```powershell
$d = "$env:ProgramData\SoftafriqueBackupAgent"
icacls $d /inheritance:r /grant:r "SYSTEM:(OI)(CI)F" "BUILTIN\Administrators:(OI)(CI)F"
```

**3. Register the service** (the MSI normally does this; `install` is the manual
fallback):

```powershell
& "C:\Program Files\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" install
```

On a re-install the service already exists, and this exits non-zero with
`CreateService: ... already exists`. That is expected and safe to continue past.
To have it finish cleanly instead, run `uninstall` first and then `install`:
neither touches `credentials.dat`.

**4. Enroll.** The token goes in an **environment variable** (preferred) or a
**file** passed with `-token-file`, which the agent deletes once consumed. It is
never in `config.yaml`. Do not use `-token`: a token on a command line is readable
by every local process.

```powershell
$env:SOFTAFRIQUE_ENROLL_TOKEN = "<one-time token>"
& "C:\Program Files\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" enroll
Remove-Item Env:\SOFTAFRIQUE_ENROLL_TOKEN
```

**5. Start the service, then verify:**

```powershell
Start-Service SoftafriqueBackupAgent
& "C:\Program Files\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" status -short
# optional single run, normally the schedule does this:
& "C:\Program Files\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" backup
```

**Re-enrolling a device the gateway already knows** (FILESERVER01). `enroll`
returns `409` for an enrolled device by design, so it will not move one on its
own: run `unenroll` first, then `enroll` with a fresh token, then confirm
`snapshots` still lists the earlier history — the gateway promises the same
escrowed key back, and an empty list is the one failure with no recovery. That
sequence is Part 5 of [`JOINT-SESSION.md`](JOINT-SESSION.md).

**What `config.yaml` needs now.** One correction to your message: the **password
is gone, but `repo:` is not gone**. The template still ships
`repo: "rest:https://backup.softafrique.net/{device_id}"`, and it is only
consulted on a device that is **not enrolled** — for local development and for
`reimport` after a reimage. On an enrolled device the repository the gateway
returned at `/enroll` always wins, so the line is inert and cannot point a paying
customer anywhere wrong. Keep it; just never put a password in it. The keys that
matter for the install are `server`, `data_dir`, `status_file`, `log_file`,
`include` (the folder to protect), and `allow_unc` (leave `false` unless a share
was opted in). `render-config` fills `data_dir`, `status_file`, `log_file` and
`include` for you. There are no credentials in the file and there must never be.

## 5. Findings log — agent-side checks (Parts 1-4 and 7)

Part 6 is yours and done (18/18), and the gateway side of Part 5 is done. This is
the agent half: **Parts 1, 2, 3, 4 and 7**. It needs a Windows Server 2019 machine
with a real optical drive and, for Part 2, a domain join; there is no way to run
it from the agent's build host, which is why it is split. Commands and expected
values are below; fill the **Result** column as you go. Part 5's live
re-enrollment run (0.2.0 against the live gateway, no MSI) is separate and is
being done by hand.

### Part 1 — DVD drive

```powershell
Get-Volume -DriveLetter D | Format-List
# confirm D: is the optical drive; a fixed disk at D: proves nothing

& "$env:ProgramFiles\Softafrique Backup Agent\validatepath.exe" 'D:\CustomerData'
$LASTEXITCODE   # 7

msiexec /i SoftafriqueBackupAgent.msi /qn /l*v dvd.log BACKUPPATH="D:\CustomerData"
$LASTEXITCODE   # non-zero
# read dvd.log: the deferred-action error must name the path and say it is an
# optical drive, not just "Custom action returned error 7"

& "$env:ProgramFiles\Softafrique Backup Agent\validatepath.exe" 'C:\SoftafriqueBackup'
$LASTEXITCODE   # 0, and the folder is created
```

### Part 2 — network share and the opt-in

```powershell
# no opt-in: refused, before anything touches the network
msiexec /i SoftafriqueBackupAgent.msi /qn /l*v share.log BACKUPPATH="\\FILESRV\CustomerData"
$LASTEXITCODE   # non-zero; share.log says "network share"

# same answer for a name that cannot resolve -> the check is not asking the share
msiexec /i SoftafriqueBackupAgent.msi /qn /l*v share2.log BACKUPPATH="\\no-such-server-abc123\share"
$LASTEXITCODE   # identical to the line above

# on FILESRV, as admin: grant the client machine account, and note the name
icacls 'D:\CustomerData' /grant "$($env:COMPUTERNAME)\SYSTEM:(OI)(CI)M"

# with the opt-in: install must succeed
.\scripts\rmm\install.ps1 -MsiPath .\SoftafriqueBackupAgent.msi `
  -BackupPath '\\FILESRV\CustomerData' -AllowUNC -EnrollToken '<token>'

# the opt-in must land in BOTH places, written by different steps
Select-String -Path "$env:ProgramData\SoftafriqueBackupAgent\config.yaml" -Pattern 'allow_unc'
# allow_unc: true

# a backup actually lands, as the service, with a file in the share to lose
& "$env:ProgramFiles\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" backup
Get-Content "$env:ProgramData\SoftafriqueBackupAgent\status.json" -Raw

# revoke the machine-account grant and run again: must fail visibly
```

### Part 3 — the folder that disappears

```powershell
$protected = 'D:\CustomerData'
New-Item -ItemType File -Path "$protected\precious.txt" -Value 'important'
Remove-Item -Recurse -Force $protected
& "$env:ProgramFiles\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" backup
Get-Content "$env:ProgramData\SoftafriqueBackupAgent\status.json" -Raw
```

Expect: folder back, `precious.txt` **not** in it; `last_attempt_status` is
`recreated`, not `success`; `last_success` still points at the previous real
backup; `consecutive_failures` unchanged.

### Part 4 — service, identity, ACLs, secret leaks

```powershell
Get-Service SoftafriqueBackupAgent          # running when a token was supplied; no-token installs exit as not enrolled
Get-Content "$env:ProgramData\SoftafriqueBackupAgent\status.json" -Raw

icacls "$env:ProgramData\SoftafriqueBackupAgent"
# SYSTEM + Administrators full control, NO Users read, inheritance disabled
icacls "$env:ProgramData\SoftafriqueBackupAgent\credentials.dat"
# after an upgrade this must be SYSTEM + Administrators too, not an old inherited Users ACE

Select-String -Path "$env:ProgramData\SoftafriqueBackupAgent\status.json" -Pattern 'rest:'
Get-Content "$env:ProgramData\SoftafriqueBackupAgent\agent.log" -Tail 100 | Select-String 'password|key'
# both MUST return nothing; a match is stop-the-line
```

Also: the 0.1.0 plaintext `password_file` is deleted when it lives inside the
data directory, and **left alone** when an operator put it elsewhere — test both.

### Part 7 — installer UI (only if someone is clicking through)

```powershell
msiexec /i SoftafriqueBackupAgent.msi
```

Expect the folder prompt, defaulting to something sensible. If the `<Publish>`
that inserts it does not take effect the failure is benign — no prompt, install
behaves as before — so do not spend the session on it.

### Results

| # | Check | Expected | Result | Follow-up |
| --- | --- | --- | --- | --- |
| 1.1 | `D:` confirmed optical | drive type optical | | |
| 1.2 | `validatepath 'D:\CustomerData'` | exit 7 | | |
| 1.3 | MSI install to `D:` | non-zero, log names path + optical | | |
| 1.4 | `validatepath 'C:\SoftafriqueBackup'` | exit 0, folder created | | |
| 2.1 | MSI share path, no opt-in | refused, "network share" | | |
| 2.2 | unresolvable share name | identical exit code, no network IO | | |
| 2.3 | machine-account grant | logged, client name noted | | |
| 2.4 | `install.ps1 -AllowUNC` | succeeds | | |
| 2.5 | `allow_unc` in config.yaml | `true` | | |
| 2.6 | backup lands on share | snapshot in status.json | | |
| 2.7 | grant revoked, backup again | fails visibly | | |
| 3.1 | folder deleted then backup | recreated, file absent | | |
| 3.2 | `last_attempt_status` | `recreated`, not `success` | | |
| 3.3 | `last_success` | unchanged | | |
| 3.4 | `consecutive_failures` | unchanged | | |
| 4.1 | service, no token | stopped, not enrolled, no restart loop | | |
| 4.2 | data-dir ACL | SYSTEM+Admins, no Users, inherit off | | |
| 4.3 | status.json secret scan | no match | | |
| 4.4 | agent.log secret scan | no match | | |
| 4.5 | 0.1.0 password_file inside data dir | deleted | | |
| 4.6 | 0.1.0 password_file outside | left alone | | |
| 7.1 | installer UI folder prompt | appears, sensible default | | |

### Things provable now, without the session

Not a substitute for the live run, but these are already settled by source and CI
and should not be re-litigated on the machine:

| Check | How it is known | Result |
| --- | --- | --- |
| `validatepath` returns 7 for a share and 7 for an unresolvable share without touching the network | `volume_windows_test.go`, and the CI real-filesystem exercise | PASS (CI) |
| The MSI validates `BACKUPPATH` before install and rolls back on failure | `Product.wxs` deferred `ValidateBackupFolder`, `Return="check"` | PASS (source) |
| A deleted protected folder is recreated and reported `recreated` | `TestDeletedProtectedFolderIsRecreatedAndNotAFailure` | PASS (CI) |
| `status.json` cannot carry a secret | `TestStatusFileNeverContainsSecrets`, `TestMarshalledStatusContainsNoSecret` | PASS (CI) |
| A share never reaches restic without the opt-in | `TestANetworkShareIsRefusedBeforeResticIsGivenThePath` | PASS (CI) |
| The opt-in is off in every shipped config | `TestTheOptInIsOffInEveryShippedConfig` | PASS (CI) |

## 6. Behaviour decisions — confirmed

Keeping `/config` as it is. A 5xx or a network error keeps the cached config and
continues to back up; a device protecting data through a gateway outage is worth
more than a device that stops because your server had a bad ten minutes. We are
not failing closed. The only things that stop backups are deliberate gateway
signals: a successful reply that is not `active`, or a `401`/`403`.

And we agree on the liveness point. Because there is no alerting yet, `POST
/status` is the only proof a device is alive, and it fires after every attempt and
at least every `status_heartbeat` while idle. The gateway contract already says
the dashboard keys off `POST /status` and that no `/heartbeat` endpoint will be
added. The scenario you hit — a server-side fault locking the gateway out of a
healthy repository, found by chance before a demo — is exactly the one liveness
alerting has to catch, and it is why the field this matters for is the one that
keeps posting when backups are quiet.

## 7. Corrections and known issues

Things this reply turned up; the first four are fixed in this repo now.

1. **"repo and password are gone from the config."** The password is gone; `repo:`
   is still shipped and is only consulted when unenrolled (dev / `reimport`). On
   an enrolled device the gateway-returned repository wins. Section 4 has the
   detail.
2. **ACL wording was inconsistent across our docs.** `reply-to-mohsin.md` claimed
   `Users` read; `OPERATIONS.md` and the installer grant only SYSTEM and
   Administrators. The `Users` read attempt was removed because a non-inheritable
   directory ACE does not reach the files inside. Docs now agree; the `.wxs` sets
   both ACEs inheritable but does not emit an explicit inheritance-block flag, so
   Part 4 item 2 must still confirm it on the machine.
3. **`JOINT-SESSION.md` Part 4 item 3 had a stop-the-line reversal** — it said
   `agent.log` *must have* the secrets. Corrected to *must contain none*.
4. **`uninstall.ps1` used the `UpgradeCode` as its `msiexec /x` product code.**
   Confirmed, and worse than "may": the GUID in the script was the `UpgradeCode`
   from `Product.wxs` verbatim, and `Product.wxs` declares no `ProductCode`, so
   WiX generates one on every build — the value cannot be hardcoded at all, and
   `msiexec /x` with it returned `1605`, which the script treated as success
   while leaving the service and the program files in place. Fixed: the script
   now looks the product code up in the Uninstall registry keys at run time
   (`Win32_Product` is avoided — its consistency check across every installed
   MSI will time out inside an RMM job), and `-MsiProductCode` survives only as
   an override for a machine where the lookup itself fails. The MSI smoke test
   in CI now runs `uninstall.ps1` and asserts the service and files are actually
   gone, because the exit code alone is exactly what hid this.
5. **`install.ps1`'s `-TimeoutSeconds` is declared but unused** (the status wait
   is a hardcoded 60 s). Cosmetic, but it should either be wired up or dropped.
