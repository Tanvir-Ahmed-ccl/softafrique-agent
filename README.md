# Softafrique Backup Agent

A Windows service that backs up a customer's folders to an append-only
[restic](https://restic.net) repository on `backup.softafrique.net`, encrypts
them with a key escrowed by our gateway, and reports every attempt back to the
dashboard.

This is version 0.2.0. It replaces 0.1.0, which could not be pointed at a real
customer safely: it took the repository password as a command-line argument, it
wrote a second copy of that password into `status.json`, it ran `restic init`
after any failure to list snapshots, and it reported a run that had never
happened as a success. Those are fixed here, and each fix has a test named after
the failure it prevents.

## How a device is set up

```
   dashboard issues a one-time token
              |
              v
   agent enroll --token ...   --->  POST /enroll     (no auth, one-time)
              |                        returns device_id, device_password,
              |                                encryption_key, repository
              v
   credentials.dat  (DPAPI, machine scope, ACL'd by the MSI)
              |
              +--> restic --host <device_id>  RESTIC_PASSWORD=<encryption_key>
              +--> HTTP Basic <device_id>:<device_password>  ->  GET /config
                                                                   POST /status
```

The device id the gateway assigns is the only identity the agent uses, and it is
the value passed to `restic --host`. It is never derived from the machine name,
because a machine name changes when a PC is renamed or reimaged and a customer
whose `device_id` drifted would appear in the dashboard as a new, unbacked-up
device.

## Design decisions worth knowing

**The gateway is append-only.** The agent contains no `forget` and no `prune`
call, and `TestArgsNeverContainRetentionCommands` asserts their absence by
reading the source. A PC that is fully compromised by ransomware still cannot
delete its own backups. Retention is a server-side decision, made from a
trusted machine with a different credential.

**The repository key never touches disk in the clear, and never a command
line.** `POST /enroll` returns it, the agent seals it with DPAPI in *machine*
scope (the service runs as LocalSystem, so there is no user profile to scope
to), and hands it to restic through the child process environment. The real
protection is the directory ACL that the MSI sets on
`%ProgramData%\SoftafriqueBackupAgent`: DPAPI only stops other users, and
anything running as SYSTEM or an administrator can still decrypt. The agent logs
a warning if it finds its data directory somewhere else.

**`device_password` and `encryption_key` are different secrets.** The first is
HTTP Basic for the API; the second is the restic repository password. The 0.1.0
build used the machine name as the Basic username and had no way to talk to the
API at all. Conflating them would mean one leaked password gives an attacker both
the API and the data, so they are stored, compared and used separately.

**A gateway outage must not stop backups.** `GET /config` failing on a network
error or a 5xx leaves the last known configuration in place and the backup
proceeds, because data protection must not depend on the control plane being up.
Only a *successful* reply that says the device is not `active` withholds
backups, since that is a deliberate act by an operator rather than a network
hiccup. A rejected credential (401/403) is also terminal: there is nothing to
retry, and the status file says `re-enroll`.

**Elapsed time is not restic's time.** A backup that waited three and a half
minutes for the network and then worked for one second took three and a half
minutes. `status.json` records `last_duration_seconds` (wall clock, measured by
the agent) and `restic_duration_seconds` separately, so a dashboard cannot show a
customer a one-second backup that took four minutes.

**Last success and last attempt are different fields.** A device that has been
failing for a week must still be able to answer "when was I last actually
protected?", so a failure moves `last_attempt_*` and
`consecutive_failures` and leaves `last_success` alone.

**An unrecognised device state is not permission.** Any `status` from
`GET /config` that is not `active` stops backups. Defaulting to "unknown means
carry on" would let a decommissioned device keep writing.

**`restic init` is off by default.** 0.1.0 ran it after any failure to list
snapshots, so a wrong device id created a brand new empty repository at the
wrong path and split the customer's history in two. Now init happens only on
restic's exit code 10 (repository missing) and only if `auto_init` says so.

**There is exactly one place an attempt is recorded.** Both the scheduled loop
and a hand-run `agent backup` go through the scheduler's attempt bookkeeping, so
a backup started by hand lands in `status.json` exactly like a scheduled one.

**One agent at a time.** A named mutex (`Global\SoftafriqueBackupAgent.
SingleInstance`) keeps a hand-run `agent backup` from racing the service onto the
same repository under the same `--host`.

## Layout

| Path | What lives there |
| --- | --- |
| `cmd/agent` | The CLI: `enroll`, `unenroll`, `reimport`, `run`, `service`, `backup`, `selftest`, `status`, `snapshots`, `restore` |
| `cmd/validatepath` | Small native helper the MSI calls to validate `BACKUPPATH` |
| `internal/agent` | Orchestration: enrollment, config sync, status reporting, suspension, 0.1.0 migration |
| `internal/api` | Gateway client: enroll, config, status, health, error classification |
| `internal/config` | Config file, defaults, validation |
| `internal/identity` | Hostname and OS caption, with the Server 2016/2019 "Windows 10" fix |
| `internal/lock` | Single-instance mutex |
| `internal/restic` | restic invocation, exit-code handling, stats, restore |
| `internal/scheduler` | When to run, retry with jitter, terminal classification |
| `internal/secret` | DPAPI-sealed credentials |
| `internal/service` | Windows service wrapper |
| `internal/status` | `status.json` schema v2, atomic writes, v1 migration |
| `installer/` | WiX v5 MSI |
| `scripts/rmm/` | Tactical RMM install, health-check and uninstall scripts |
| `docs/` | Operations runbook, deliverable prose, reply to Mohsin |

## Building

```bash
make test                 # unit tests
make vet                  # vet, including GOOS=windows
make build-mac            # developer binary
make build-windows        # agent.exe + bundled restic.exe
make msi VERSION=0.2.0    # unsigned pilot MSI (needs wix on PATH)
```

`make build-windows` fetches `restic.exe` once and leaves it in `build/bin`, so
the customer PC needs nothing else installed.

## Installing on a customer machine

1. Deploy the MSI from Tactical RMM with the `ENROLLTOKEN` property set to a
   one-time token from the dashboard. The MSI runs enrollment itself and never
   writes the token to disk.
2. Confirm with the health-check script:

```powershell
.\scripts\rmm\health-check.ps1 -AgentDir "$env:ProgramData\SoftafriqueBackupAgent"
```

3. `status.json` is the machine-readable state. It contains no secrets.

For a manual install, `agent enroll` reads the token from
`SOFTAFRIQUE_ENROLL_TOKEN` or `-token-file` rather than `-token`, because a token
on a command line is readable by every local process through the process table.

## Configuration

See `config.example.yaml`, which is commented for an operator. There are no
credentials in the config file, and the device id is not configurable on an
enrolled device.

## Status file

```jsonc
{
  "schema_version": 2,
  "device_id": "dev-01HQ8",
  "tenant": "acme",
  "hostname": "FILESERVER01",
  "os_caption": "Windows Server 2019 Standard 10.0.17763",
  "enrolled": true,
  "server_status": "active",
  "last_attempt_status": "success",   // running | success | failed | suspended
  "last_attempt_start": "2026-10-08T09:00:00Z",
  "last_success":      "2026-10-08T09:04:12Z",
  "last_duration_seconds": 252,        // wall clock
  "restic_duration_seconds": 12,       // restic's own number
  "consecutive_failures": 0,
  "backup_paths": ["C:\\CustomerData"],
  "repo_host": "backup.softafrique.net", // never a full URL
  "repo_stats": { "repo_bytes": 0, "snapshot_count": 0, "captured_at": "" }
}
```

`last_backup_status` and `last_backup_time` are kept for monitoring scripts
written against 0.1.0. A 0.1.0 `status.json` is migrated on read: the
credential-bearing `repo` field is dropped, and the file is rewritten as v2.

## Testing

```bash
go test ./...
GOOS=windows go build ./...
```

The Windows-only paths (DPAPI, the named mutex, the service wrapper) are
compiled and vetted with `GOOS=windows` on every change, but their runtime
behaviour can only be exercised on a real Windows host. The MSI and RMM scripts
are validated by hand against a Windows Server 2019 and a Windows 10 test
machine; see `docs/OPERATIONS.md`.
