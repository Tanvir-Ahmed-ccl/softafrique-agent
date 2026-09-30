# Gateway contract

What the backup agent sends, what it expects back, and the decisions on your
side that change what it does.

- Machine-readable spec: [`gateway-api.openapi.yaml`](gateway-api.openapi.yaml)
- Authoritative source: `internal/api` (`client.go`, `api.go`). If this document
  and the Go structs ever disagree, the Go structs are the behaviour and the
  document is the bug.
- Base URL: `https://backup.softafrique.net/api/v1`

---

## Read this first: three things that are not what they look like

If you have been reading the wrong artefact, these will save you a debugging
session.

### 1. `server_status` is not a gateway field. It is `status`.

The `GET /config` reply field is **`status`**. `server_status`, `repo_stats` and
`schema_version` are all fields of `status.json` — the local monitoring file on
the device that Tactical RMM reads. The agent never sends any of the three to the
gateway.

So the good news: the field name you are already returning is correct. Only the
vocabulary is wrong, and the fix is one line.

### 2. There is no `POST /heartbeat` endpoint.

The agent has four endpoints: `/enroll`, `/config`, `/status`, `/health`.

"Heartbeat" is an internal *cadence*, not an endpoint. The agent re-posts
`POST /status` with the last-known state every `status_heartbeat` (default 6
hours) while idle, and after every attempt.

**Confirmed: point dashboard liveness at `POST /status`.** Every device reports
after every attempt and at least every 6 hours, so liveness can be keyed off it
directly. This was the item most likely to have made the pilot look like a total
failure — a dashboard hung off a `/heartbeat` route would have read every device
as offline while the backups were in fact succeeding. No `/heartbeat` endpoint
will be added; there is nothing for it to do that `POST /status` is not already
doing.

### 3. `reimport` never talks to the gateway.

Re-import is entirely local. It exists for a machine that was reimaged, where
the DPAPI blob went with the disk. The operator obtains the escrowed key from the
dashboard and hands it to the agent as a file:

```
SoftafriqueBackupAgent.exe reimport \
  -key-file C:\Temp\key.txt \
  -device-password-file C:\Temp\pass.txt \
  -config "%ProgramData%\SoftafriqueBackupAgent\config.yaml"
```

Both files are required, and the repository comes from the **local config**, not
from the gateway — re-import exists for the case where the gateway cannot be
reached, so asking it would defeat the purpose.

**What proves the device is entitled to the escrowed key: nothing, at the
protocol level.** There is no challenge and no signature. Entitlement is proven
afterwards, by the repository refusing to open with the wrong key. That is a
deliberate decision, documented in
[`OPERATIONS.md`](OPERATIONS.md#what-is-not-automated-yet), but it does mean a
human's hands transit the key. It is why `reimport` is not in the RMM scripts
and requires `-yes`.

---

## Suspended versus revoked

These are different problems with different fixes, so the agent records them
differently. This is the part most worth getting right. `archived` behaves
exactly like `revoked` and is not discussed separately.

| | **Suspended** | **Revoked / archived** |
| --- | --- | --- |
| Gateway says | `200` + `{"status":"suspended"}` | **`403`** |
| Credentials | still valid | dead |
| Agent does | stops backing up, keeps credentials | stops, marks itself revoked |
| Local `server_status` | `suspended` | `revoked` |
| Local `action_required` | `gateway reports device status "suspended"` | `re-enroll: the gateway has withdrawn this device` |
| How it ends | you set `active`; the device resumes by itself | new token, `agent unenroll`, re-enroll (same escrowed key) |

### Suspend a device with 200 and a status value. Revoke it with 403.

Returning `{"status": "revoked"}` *does* suspend the device, but the agent
records the reason as `gateway reports device status "revoked"` and does not ask
for a re-enrollment — which is the part a human needs. Revoke with **403**, as
confirmed.

### Never implement "suspend" as a 5xx.

A transport error or a 5xx on `GET /config` deliberately keeps the cached
configuration and **keeps backing up**. That is the right behaviour during an
outage, and it means the agent cannot tell an outage from a decision. If
suspending is a 500, the device carries on protecting data after you meant it to
stop.

### Status code map

| Code | `/enroll` | `/config` and `/status` |
| --- | --- | --- |
| `2xx` | credentials issued | device may back up |
| `400` | rejected; message shown to the technician | counted as a config failure; last config kept |
| `401` | **avoid** — see below | device marked **revoked** |
| `403` | **token invalid, expired or spent** | **device marked revoked** (`revoked` and `archived` both) |
| `404` | rejected | counted as a config failure |
| `409` | **already enrolled** | rejected |
| `5xx` | retried later | **last config kept, backups continue** |

All of this is now confirmed. Two consequences worth writing down:

- **`403` on both `/config` and `/status` is how a device is revoked or
  archived.** Because `/status` returns it too, a withdrawal takes effect on the
  very next attempt rather than up to 15 minutes later when the agent happens to
  call `/config`. That answered an open question and it is the behaviour we asked
  for.
- **`401` on `/enroll` is the one trap.** The agent maps it to "credentials
  rejected", which is meaningless before enrollment exists, and the operator gets
  a generic error with no recovery advice. Use **`403`** for a bad token and
  **`409`** for one already used. Both are confirmed.

---

## The calls

### `POST /enroll` — no auth, once per device

```json
{
  "token": "smtl_9f2c1d4e7a8b4c0f",
  "hostname": "DESKTOP-ABC",
  "os_caption": "Windows Server 2019 Standard",
  "backup_path": "D:\\CustomerData",
  "agent_version": "0.2.0"
}
```

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

**The repository is exactly `rest:https://backup.softafrique.net/{device_id}`** —
confirmed, and the agent passes the string to restic as-is. No `rest:` prefixing
by us, no `/repos/` or `/restic/` path element, no userinfo, and **no trailing
slash**. A trailing slash is not cosmetic: restic treats `.../dev-1` and
`.../dev-1/` as the same repository but writes the path into snapshot
identifiers, so a difference shows up as a second apparent host in the history.

**`device_id`, `repository`, `device_password` and `encryption_key` are all
required.** A reply missing any of them is rejected outright rather than
half-applied, because a device with a repository but no key cannot back up, and a
device with a key but no repository cannot find it.

Two constraints the agent enforces on the reply:

- **`device_id` must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`.** A UUID with
  anything else in it, a space, or a leading punctuation mark is refused at
  enrollment rather than at the first backup.
- **`repository` must not embed credentials.** A `user:pass@` section is refused
  with an explicit error, because a URL carrying a password is world-readable in
  a process command line. Any password or query string is stripped before the URL
  is stored or logged. `device_password` is a separate field and is never
  concatenated into the repository URL — that separation is what makes it
  revocable on its own.

**Confirmed, and this is the property the agent is built on:**

1. **`/enroll` creates the repository**, and **fails if its own `restic init`
   fails.** The agent therefore has no `restic init` code path at all — not
   defaulted off, absent. A repository that is missing is terminal and is
   reported as such, because the alternative (the 0.1.0 behaviour) is a mistyped
   device id silently creating a second empty repository and splitting a
   customer's history in two.
2. **`/enroll` never re-enrolls.** An already-enrolled device gets **`409`**. The
   only route to a fresh enrollment is unenroll first, and then `/enroll` returns
   the **same escrowed key**. So a device that *was* enrolled can be re-enrolled
   without a new key, and a key change can never orphan its existing snapshots.

   Worth being precise about, because it is easy to over-read: this does **not**
   apply to a 0.1.0 device. 0.1.0 never talked to the gateway, so the gateway has
   no escrowed key for it and its first 0.2.0 enrollment mints one. There is no
   history to orphan, because there is no gateway-side history. See
   [`OPERATIONS.md`](OPERATIONS.md#upgrading-010-to-020).
3. **`device_id` is permanent.** It appears in existing snapshot filenames, so a
   device that changed it would orphan its own history.


The agent then *proves* the reply works — it calls `GET /config` with the new
credentials and opens the repository with the issued key — before committing
anything to disk. A bad reply fails at enrollment, with the token still in hand,
rather than on tomorrow's scheduled run.

### `GET /config` — HTTP basic (`device_id` / `device_password`)

```json
{
  "device_id": "dev-01HQ8K2M4P",
  "backup_path": "D:\\CustomerData",
  "status": "active",
  "schedule_interval": "1h",
  "retry_interval": "15m"
}
```

**Every value `status` can take, and every value it can *be*:**

There are four device states, and they do **not** all arrive the same way. This is
the single most important thing on the page, because two of the four are
signalled by a status code and not by a body:

| State | How it reaches the agent | Agent's reading |
| --- | --- | --- |
| `active` | `200` + `{"status":"active"}` | may back up |
| `suspended` | `200` + `{"status":"suspended"}` | suspended, with that reason recorded |
| `revoked` | **`403`** — no body worth reading | marked revoked, asks for re-enrollment |
| `archived` | **`403`** — no body worth reading | treated as revoked, asks for re-enrollment |

`/config` therefore only ever carries `active` or `suspended`.

A device that is
`revoked` or `archived` is not described in a body at all — it is told by the
`403` on `/config` **and** on `/status`. That is deliberate on your side and
convenient for us: a status value of `"revoked"` with a `200` would suspend the
device but would *not* tell it the credentials are dead, so it would sit there
holding a key it can no longer use and never ask for a new one.

The remaining cases, which are all treated as suspended because an unrecognised
value must not silently keep a decommissioned device writing to the repository:

| `status` | Agent's reading |
| --- | --- |
| anything else (`"inactive"`, `"paused"`, a value you add later) | suspended |
| `""`, `null`, or the field missing | suspended, reason: *"gateway returned no status field; treating the device as suspended"* |

Only `active` permits a backup, and the comparison is exact and
case-sensitive — `"Active"` suspends.

Unknown response fields are ignored, so you can add fields without breaking older
agents.

`schedule_interval` and `retry_interval` accept either `"1h"` or a bare number
of seconds (`3600`). Omit them to leave the local value unchanged.

### `POST /status` — HTTP basic

Sent after every completed attempt, and on the heartbeat timer.

```json
{
  "last_backup_status": "success",
  "last_snapshot_id": "a1b2c3d4",
  "files_added": 1284,
  "files_changed": 12,
  "bytes_added": 734003200,
  "last_duration_seconds": 41.2,
  "last_backup_error": "",
  "agent_version": "0.2.0",
  "os_caption": "Windows Server 2019 Standard"
}
```

That is the complete body — **nine fields, no more.** The richer monitoring view
(tenant, OS, backup paths, repository usage, last attempt versus last success)
lives in `status.json` for Tactical RMM, because adding undeclared fields here
would mean guessing at your schema. If the dashboard wants more, that is a change
on your side and we will add it deliberately.

**Repository usage is not in it, and is not coming.** Confirmed: `/status` does
not take usage. The agent computes `restic stats` locally and writes it to
`status.json`. The reason we are comfortable leaving it out is that we could not
make it trustworthy over the wire anyway: a usage figure is a point-in-time
reading, and a body with no `captured_at` cannot be distinguished from a stale
one. It is in `status.json` for the dashboard, where Tactical RMM already knows
how stale a reading is.

`last_backup_status` is contractually only `success` or `failed`, and a report is
only sent after an attempt has finished. There is no `running` value because a
report is only sent once the attempt is over.

**A run that made no attempt sends no request at all.** A suspended device, a
revoked one, an unenrolled one, and a device whose folder the agent has refused
to protect all skip without POSTing. Sending `failed` for a run that never tried
would inflate a failure count with devices that are working exactly as intended —
a decommissioned machine and a genuinely broken one would look identical. The
reason is recorded locally in `status.json` as `last_attempt_error` instead, and
it is the line a support engineer reads.

**One case arrives as `failed` even though nothing errored.** If the protected
folder had been deleted, the agent recreates it and the backup *succeeds* — but
the snapshot it just took is of an empty directory, so reporting `success` would
be a lie that a device could use indefinitely. It is sent as `failed` with
`last_backup_error` saying the folder was missing and had to be recreated.
`status.json` distinguishes it properly as `last_attempt_status: recreated`, and
`health-check.ps1` calls it out by name, because "the customer deleted their data
folder" and "the backup broke" are very different tickets. Worth knowing before
the first support call: a `failed` with that message means the *agent* is fine.

`last_duration_seconds` is wall-clock time **including** the wait for the
network. restic's own duration is not in this contract; it is in `status.json` as
`restic_duration_seconds`.

The agent reads no response body, so any 2xx works.

### `GET /health` — no auth

Any 2xx is healthy. A failure is logged as a warning and never blocks enrollment
or a backup: the gateway API and the repository endpoint are different services,
and being unable to reach one must not stop data protection.

---

## Two things that are not on this wire

Both are fields of the local `status.json` on the device. The agent never sends
them, and your dashboard should read them from Tactical RMM rather than expect
them from the gateway.

- **`schema_version`** — the layout version of `status.json` (currently `2`). A
  local file-format marker. Meaningless over the API.
- **`repo_stats`** — `{repo_bytes, restore_bytes, file_count, snapshot_count,
  captured_at}`. Taken by the agent from restic every 30 minutes and written to
  `status.json`, and read from there by the dashboard. Confirmed as not part of
  `/status`: the agent could not make it trustworthy over the wire in any case,
  because a usage figure with no `captured_at` cannot be told apart from a stale
  one.

---

## Closed questions

These were open in the deliverable draft and are all answered. They are kept,
with the answers, because "we asked and this is what came back" is more useful to
the next reader than a silent deletion — and because two of the answers are
load-bearing enough that a future change that contradicts them would be a bug.

### 1. Does the dashboard need a distinct liveness endpoint? — **No. Use `POST /status`.**

Every device reports after every attempt and at least every 6 hours. No
`/heartbeat` will be added.

### 2. Can `POST /status` also return 401/403? — **Yes, and it does.**

A withdrawn device gets `403` from `/config` **and** `/status`, which is how
`revoked` and `archived` are signalled. A withdrawal therefore takes effect on
the next attempt, not up to 15 minutes later.

### 3. `/enroll` semantics — **all four confirmed.**

1. **It creates the repository**, and fails if its own init fails. The agent has
   no init code path at all.
2. **It never re-enrolls**: `409` for an enrolled device. After an explicit
   unenroll, `/enroll` returns the **same escrowed key** — so an upgrade needs no
   new token, and a changed key can never orphan existing snapshots.
3. **`repository` comes back complete**, as
   `rest:https://backup.softafrique.net/{device_id}`. The agent passes it to
   restic as-is.
4. **`device_id` is permanent**, as it appears in existing snapshot filenames.

### 4. Repository usage in `/status`? — **No, and it stays local.**

`/status` does not take it. The agent reads `restic stats` into `status.json`,
where `captured_at` makes a stale reading identifiable. A usage figure without a
timestamp on the wire is a figure nobody can trust.

### 5. Fixed-disk policy for the protected folder — **fixed disks only, with a share opt-in.**

Agreed policy, and it is implemented in one rule applied in two places
(`internal/pathpolicy`):

| Volume | Behaviour |
| --- | --- |
| Fixed disk | allowed |
| **Network share** (`\\fileserver\share\customer`) | **refused by default.** Allowed only behind an explicit opt-in: `install.ps1 -AllowUNC` at install time, or `allow_unc: true` in `config.yaml` at run time. |
| Optical (CD/DVD) | refused. No opt-in. |
| Removable (USB, memory card) | refused. No opt-in. |
| RAM disk, unmounted drive letter, anything unrecognised | refused. No opt-in. |

**On removable drives:** we considered this and it is refused. An ejected USB
drive is not a misconfiguration to be tolerated — it is a backup that stops
existing without anything saying so, and a customer whose data silently stopped
being protected is worse off than one whose install was refused.

**On UNC paths:** yes, in scope, and restic handles them. They are refused by
default for a reason that is not obvious from the outside: the service runs as
**LocalSystem**, so it reaches a share as the *machine account*, not as the
technician who opened it. `\\SERVER\CustomerData` can work perfectly during
installation and then fail every hourly backup because the machine account was
never granted access, with nothing in a backup report to say where. So the case
is allowed — but only when a person has said so, out loud, in the install command
or the config file.

**The agent does not create an SMB share.** The share must already exist and
already be shared, and the machine account must already have access. The
installer does not offer to create one either; that is the one open idea in this
section and nothing depends on it.

The refusal applies to the path the *gateway* reports, not only to what a
technician typed at install time. It happens before restic is handed the path,
it is terminal rather than retried every five minutes, and the reason is written
into `status.json` naming the paths and the volume types.

---

## Still open

Nothing that blocks the pilot. Two things we would like, and neither is a
question you have to answer for the next release:

1. **Should the MSI offer to create and share a folder?** Decided against for now.
   It is a lot of new failure modes — share permissions, NTFS permissions, the
   machine account — for a minority of sites, and the opt-in already covers them
   with less to go wrong.
2. **`schedule_interval` and `retry_interval` on `/config`.** The agent accepts
   both `"1h"` and bare seconds and is happy with either, so this is only worth
   closing if you want the values to be authoritative.
