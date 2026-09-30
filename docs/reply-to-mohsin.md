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
  `%ProgramData%\SoftafriqueBackupAgent` to SYSTEM + Administrators full control
  and `Users` read. Inheritance is disabled so a later widening of
  `%ProgramData%` cannot silently expose it.
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

- `last_attempt_status` (`running`, `success`, `failed`, `suspended`) and
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
