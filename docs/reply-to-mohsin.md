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
- `restic init` now runs **only** on restic exit code 10 (repository missing) and
  only if `auto_init` is explicitly enabled, which by default it is not.

**What we need from you.** Confirmation that `/enroll` creates the repository, so
`auto_init` can stay off permanently. If the gateway does not, tell us and we will
turn it on for the first run only.

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

**What we need from you.** Written confirmation of:

1. Whether `/enroll` returns the same `encryption_key` for an existing device, or
   a new one. We will not re-enroll a working device on a hunch; if the key can
   change we need to know, because a changed key against existing snapshots means
   the history is unreadable.
2. Re-enrollment and 409 semantics, so `agent enroll` can say the right thing
   rather than guessing.
3. Whether `repository` in the reply includes the `rest:` prefix, or a bare URL we
   are expected to prefix. We currently treat whatever you send as a complete
   restic URL and strip any userinfo, so a bare URL would fail at the first
   `restic` call rather than silently.
4. Whether repository usage belongs in `/status`. We read `restic stats`
   server-side-by-CLI and put it in `status.json` for the dashboard, because
   adding an undeclared field to the `/status` contract would be guessing at your
   schema.

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
| `TestAttemptRecordsElapsedNotResticTime` | elapsed is wall clock, not restic's number |
| `TestFailedBackupKeepsLastSuccess` | a failure does not move last success |
| `TestImplausibleDeviceIDIsRejected` | a hostile device id cannot redirect the repository |
| `TestMigrationRemovesTheLegacyPasswordFile` | the 0.1.0 plaintext key is deleted |

CI runs `go test`, `go vet` for `GOOS=windows`, a native Windows test run, and an
MSI install/uninstall smoke test on `windows-latest`.
