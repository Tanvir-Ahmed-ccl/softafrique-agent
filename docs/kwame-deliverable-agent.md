# Backup agent — deliverable section (for Kwame)

Draft for the joint client-facing document, section owned by the agent build.
Written to be pasted alongside the gateway section rather than read as a technical
specification, but every claim in it is something we can demonstrate on a test
machine.

Target: **Friday 9 October 2026.** This section is complete and needs the gateway
section's four answers (below) to become final.

---

## 1. What the customer gets

One installer. A technician runs it from our RMM, types the folder to protect, and
the machine is protected within a few minutes. Nothing is asked of the customer
except access to their data folder, and nothing about the backup is visible to
them until they need a restore.

There is no account to create, no key to copy onto a USB stick, and no password
for the customer to lose. The encryption key is generated and held by our gateway
at enrollment, not derived from anything on the machine.

## 2. What is protected, and how

Customer-selected folders, encrypted client-side with AES-256 before they leave
the machine. Backups are incremental: each run adds a snapshot of what changed
since the last one, and verifies what is already there, so a full check of the
whole folder happens every run without re-sending it.

The customer can add or change folders from the dashboard without a site visit.
Where a technician has set a specific folder at install time, that folder is
protected from the first run, before the dashboard has been touched.

## 3. The property that matters most: backups cannot be deleted by the machine

This is the one to lead with, because it is what separates this from a folder
copy or a sync tool.

The backup server is **append-only**. A machine that is fully compromised — by
ransomware, by a malicious insider, by anyone who gets administrator on the PC —
can add data to its own backups and read them, and it **cannot delete a single
snapshot or any history**. There is no code in the agent that deletes anything,
and a test reads the source to confirm it stays that way.

The practical consequence: ransomware that encrypts or destroys the customer's
files does not touch the backups, and restoring does not depend on the customer's
machine cooperating.

The trade-off, stated plainly: **nothing is ever automatically removed, so storage
grows.** Retention is a deliberate decision made from a trusted machine, not by
the agent. We are treating the retention policy as a server-side item before the
fleet grows; until then, growth is the safe direction to be wrong in.

## 4. If the machine is lost or reimaged

The backups are on the server, not the machine, so losing the machine loses
nothing. Reinstalling and enrolling again gives a new machine access to the same
history.

The encryption key is escrowed by the gateway, so a restore is possible even when
the original machine and its local key are both gone. This is deliberate: it is
what makes the backups genuinely recoverable, and it is also why access to the
escrow is controlled and audited rather than handed to the installer.

## 5. What we can see, and what the customer can

**We can see**, per device: whether it is enrolled, whether the service is
running, when it last completed a backup, how long that took, how many snapshots
exist, how much space they use, and any error the agent hit. That is what the RMM
dashboard shows, and it is what our support uses to answer "is this machine
actually protected" without asking the customer.

**The status file contains no secrets.** No passwords, no keys, and not even the
full backup address — only the server's host name. It is designed to be readable
by a monitoring agent with read access and nothing more.

**The customer is not shown a dashboard** in this release. They see working
backups and a restore when they need one.

## 6. Honest limits

Stated here rather than discovered later:

- **Retention is not automated.** See section 3.
- **A machine that is off or unplugged is not backed up** during that time, and
  the dashboard shows the gap rather than hiding it. The next run catches up.
- **The pilot installer is unsigned.** Windows will warn on some machines.
  Code signing is a gate before general availability, not an open question.
- **The OS name reported was wrong on some servers.** 0.1.0 reported Windows 10
  for Server 2016 and 2019 because that is what the registry says. Fixed and
  tested against the build number.
- **We do not yet back up Exchange, SQL Server or any application-aware state.**
  This is file-level backup. Restoring a database needs the application
  administrator's procedure on top of the files.
- **Single machine at a time.** One agent per machine, enforced.

## 7. How it is deployed and supported

| | |
| --- | --- |
| Install | One MSI, deployed by our RMM, with the folder to protect |
| Uninstall | Removes the service and files. Snapshots and the escrow key are unaffected, by design |
| Health check | A script the RMM runs on a schedule; it fails when a machine stops being protected, not only when the service stops |
| Support | Agent log plus a machine-readable status file, both readable without a site visit |
| Upgrade | The installer leaves the enrolled device enrolled, so a patch cannot leave a customer unprotected |

## 8. Dates

| Milestone | Date |
| --- | --- |
| Release candidate for internal testing | Thu 8 Oct 2026 |
| Fixes from the pilot round | Fri 16 Oct 2026 |
| Installer ready for the client pilot | Fri 30 Oct 2026 |
| RMM deployment scripts ready | Fri 6 Nov 2026 |
| Upgrade validated on a live device | Fri 13 Nov 2026 |
| Code signing and GA gate | Fri 20 Nov 2026 |

## 9. What we need from the gateway side to make this section final

Four answers, all small, all blocking only this section's wording:

1. Does `/enroll` create the repository? If it does, the agent never will, which
   is what stops a mistyped device id creating a second empty repository and
   splitting a customer's history.
2. Does `/enroll` return the same encryption key for a device that is already
   enrolled? We will not re-enroll a working device without knowing.
3. Does the `repository` value in the reply include the `rest:` prefix?
4. Is repository usage reporting wanted in `/status`, or is the agent's own
   reading enough for the dashboard?

If any of these is still open at the 8 October gate, this section ships with the
agent behaviour as implemented and the gateway column marked pending, rather than
waiting.
