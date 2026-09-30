# Joint session: agent and gateway

The one document to have open while the agent and the gateway are tested against
each other on a real Windows machine. Everything in here is either something CI
cannot do or something that has not been done yet.

Bring: this file, [`gateway-contract.md`](gateway-contract.md), the 0.2.0 MSI, a
Windows Server 2019 machine you can break, and a gateway account with at least
one test tenant.

---

## Before the session: what CI has and has not proved

Worth reading first, because it decides what the session is for. CI has run:

- the whole Go suite on `ubuntu-latest`, and the whole suite **natively on
  `windows-latest`**;
- `go vet` for `GOOS=windows` and an `arm64` cross-build;
- the real `validatepath.exe` against a real filesystem, including the refusal of
  a network share;
- a real MSI install, uninstall, config-file substitution, folder creation, and a
  refused install.

CI has **not** run any of it on a machine with a DVD drive, a domain join, a
LocalSystem service that actually starts, a real file server, or a real customer
folder. The list below is exactly that gap.

---

## Part 1 — the DVD drive, and the one that started all this

This is the bug the fixed-disk check exists for: on a Server 2019 test machine
`BACKUPPATH` was pointed at `D:`, which was the optical drive. Every check the
installer could express passed, so the install succeeded and then every hourly
backup failed with *device is not ready*.

1. Confirm `D:` is the optical drive: `Get-Volume -DriveLetter D | Format-List`.

   > **If `D:` is a fixed disk on this machine, use a real optical drive or stop
   > and get a machine that has one.** The whole test is the volume type, and a
   > fixed disk at `D:` will pass for the wrong reason and prove nothing.

2. `validatepath.exe` must refuse it, with no disc in the tray:

   ```powershell
   & "$env:ProgramFiles\Softafrique Backup Agent\validatepath.exe" 'D:\CustomerData'
   $LASTEXITCODE   # 7
   ```

   Exit code 7 means "not a fixed disk". **7, not 1** — the MSI branches on it.
   With a disc in the tray it must give the identical answer, because
   `GetDriveType` reads the drive letter, not the disc.

3. Install for real and confirm the install **fails**, rather than installing and
   failing later:

   ```powershell
   msiexec /i SoftafriqueBackupAgent.msi /qn /l*v dvd.log BACKUPPATH="D:\CustomerData"
   $LASTEXITCODE   # non-zero
   ```

   Then read `dvd.log` and find the deferred-action error. **Read the sentence
   yourself.** It has to name the path and say it is an optical drive; a log that
   says only "Custom action returned error 7" is a bug, because a technician
   cannot act on that.

4. A fixed disk at the same letter is accepted — `C:\SoftafriqueBackup` should give
   exit code 0 and create the folder.

---

## Part 2 — a network share, and the opt-in

The refusal is not because restic cannot read a share. It is because the service
runs as **LocalSystem**, so it reaches a share as the *machine account*, not as
the technician who set it up. This part proves both halves of that.

1. Default, no opt-in — the install must fail:

   ```powershell
   msiexec /i SoftafriqueBackupAgent.msi /qn /l*v share.log BACKUPPATH="\\FILESRV\CustomerData"
   $LASTEXITCODE   # non-zero
   ```

   Check `share.log` says *network share*. The failure must happen **before**
   anything touches the network, so it is the same answer whether or not
   `FILESRV` exists — try a name that cannot resolve and confirm the exit code
   does not change. If it does, the check is asking the share whether it is there,
   which is the access it was supposed to be gating.

2. Grant the machine account access, deliberately, so the share is genuinely
   readable:

   ```powershell
   # On FILESRV, as an admin. $env:COMPUTERNAME is the client.
   icacls 'D:\CustomerData' /grant "$($env:COMPUTERNAME)\SYSTEM:(OI)(CI)M"
   ```

   **Log the client name in the session notes.** This grant is the thing a real
   customer will get wrong, and it is invisible from the agent.

3. With the opt-in, the install must succeed and the agent must back up to the
   share:

   ```powershell
   .\scripts\rmm\install.ps1 -MsiPath .\SoftafriqueBackupAgent.msi `
     -BackupPath '\\FILESRV\CustomerData' -AllowUNC -EnrollToken '<token>'
   ```

   Then check the two places the opt-in is recorded, because they are written by
   different steps and either can silently be missing:

   ```powershell
   Select-String -Path "$env:ProgramData\SoftafriqueBackupAgent\config.yaml" -Pattern 'allow_unc'
   ```

   `allow_unc: true` **must** be there. If the install passed and the key is
   `false`, the agent will refuse the share at run time and the install will look
   like it worked. That mismatch is the single most likely bug this session finds.

4. Confirm a backup actually lands, as the service, not as you:

   ```powershell
   & "$env:ProgramFiles\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" backup
   Get-Content "$env:ProgramData\SoftafriqueBackupAgent\status.json" -Raw
   ```

   Put a file in the share first, so there is something to lose. Then **revoke the
   machine-account grant** and run the backup again: it must fail, and it must
   fail *visibly*. This is the scenario the refusal exists for, and a share that
   works at 09:00 and silently does nothing at 22:00 is the failure we are buying
   the opt-in to avoid.

---

## Part 3 — the folder that disappears

The fix for the case where a customer's protected folder is deleted after
installation. It must be recreated, and it must **not** be reported as a green
backup, because the snapshot is of an empty directory.

```powershell
$protected = 'D:\CustomerData'   # whatever it actually is
New-Item -ItemType File -Path "$protected\precious.txt" -Value 'important'
Remove-Item -Recurse -Force $protected

& "$env:ProgramFiles\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" backup
Get-Content "$env:ProgramData\SoftafriqueBackupAgent\status.json" -Raw
```

Check all four:

- [ ] the folder is back, with `precious.txt` **not** in it (it was deleted, and
      nothing should invent it);
- [ ] `last_attempt_status` is `recreated`, not `success`;
- [ ] `last_success` still points at the *previous* real backup;
- [ ] `consecutive_failures` is unchanged — this is not a failure and it must not
      look like one.

The tempting wrong result is a green `success`. That is the 0.1.0 lie and the
reason `recreated` is a distinct state at all.

---

## Part 4 — service, identity, and the ACLs

The parts that only mean anything as LocalSystem on a domain-joined machine.

1. After install, the service must be **stopped and not enrolled** when no token
   was supplied, and running when one was:

   ```powershell
   Get-Service SoftafriqueBackupAgent
   Get-Content "$env:ProgramData\SoftafriqueBackupAgent\status.json" -Raw
   ```

   A fresh install with no token must not be in a crash-restart loop. Check the
   Windows event log for service start failures.

2. The data directory ACL is the real control on the key — DPAPI stops other
   *users*, not anything running as SYSTEM or an administrator:

   ```powershell
   icacls "$env:ProgramData\SoftafriqueBackupAgent"
   ```

   Must be SYSTEM and Administrators full control, `Users` read, and **inheritance
   disabled** so a later widening of `%ProgramData%` cannot silently expose
   `credentials.dat`. Verify the inherited ACE is genuinely gone.

3. Confirm the key is not anywhere it should not be, after a real run:

   ```powershell
   Select-String -Path "$env:ProgramData\SoftafriqueBackupAgent\status.json" -Pattern 'rest:'
   Get-Content "$env:ProgramData\SoftafriqueBackupAgent\agent.log" -Tail 100 | Select-String 'password|key'
   ```

   `agent.log` must have the token, the key and the repository password in it. A
   match in any of the three is a stop-the-line finding.

4. The 0.1.0 plaintext `password_file` is deleted when it lives inside the data
   directory, and **left alone** when an operator put it somewhere else. Test both.

---

## Part 5 — upgrade and re-enrollment

0.1.0 was never enrolled: it had no credentials blob, because it had no way to
talk to the gateway. So 0.1.0 → 0.2.0 is a **first enrollment** and needs a
token. The unenroll-then-re-enroll returns-the-same-key guarantee applies to
devices that were already enrolled on the gateway, and does not rescue a 0.1.0
device.

1. Install 0.1.0 from its own MSI, if you still have it. Put a file in the
   protected folder.
2. `Get-Content status.json` on the 0.1.0 machine and **keep a copy**. You are
   about to change several of the fields, and the point of the comparison is to
   know which changed on purpose.
3. Install 0.2.0 with a token:

   ```powershell
   .\scripts\rmm\install.ps1 -MsiPath .\SoftafriqueBackupAgent.msi `
     -BackupPath 'D:\CustomerData' -EnrollToken '<token>'
   .\scripts\rmm\health-check.ps1
   ```

4. Check: no `repo:` line and no `password_file` in `config.yaml`; no plaintext
   password file left inside the data directory; a backup runs and reaches the
   gateway.

**The re-enrollment case that does not need a new key** — worth doing because it
is the one the contract now promises:

5. On a working 0.2.0 device, destroy the local credentials and re-enroll:

   ```powershell
   & "$env:ProgramFiles\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" unenroll
   & "$env:ProgramFiles\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" enroll -token '<new token>'
   & "$env:ProgramFiles\Softafrique Backup Agent\SoftafriqueBackupAgent.exe" snapshots
   ```

   `unenroll` first, because `/enroll` returns `409` for an enrolled device and
   the agent will not re-enroll on its own. Then `snapshots` must list the
   **earlier** snapshots. If it lists none, the gateway issued a different key
   and every existing backup on that device is now unreadable — that is the one
   failure in this document with no recovery.

---

## Part 6 — the gateway contract, end to end

One device, and the whole wire walked once on a real machine.

- [ ] `/enroll` creates the repository, and `device_id` is what you expect.
- [ ] Repository is exactly `rest:https://backup.softafrique.net/{device_id}` —
      no userinfo, no trailing slash.
- [ ] Enroll the same device twice: `409`, and the agent says so in words a
      technician can act on.
- [ ] `/config` returns `active`; a backup runs.
- [ ] Set `suspended`; backups stop **within one interval**, `status.json` says
      `suspended` with the reason, and `last_success` is preserved. No failed
      report is sent for a run that never happened.
- [ ] Set back to `active`; it resumes by itself, with no reinstall.
- [ ] Suspend with a `5xx` instead — the agent must **keep backing up** and show
      the last known config. This is deliberate and it will feel wrong; confirm
      the cached-config path in `agent.log`.
- [ ] Revoke: `403` from `/config` **and** `/status`. Take effect on the next
      attempt, not at the next config poll.
- [ ] `archived`: treated exactly like revoked.
- [ ] Suspend the gateway process entirely. Backups continue. Then
      `POST /status` liveness must go stale after 6 hours — this is the item that
      would make every device read as offline if it is wired up wrong, so check it
      even though nothing is broken locally.

---

## Part 7 — the installer UI

Only if someone is clicking through it. The RMM path is `/qn` and does not care.

- [ ] The folder prompt appears and defaults to something sensible.
- [ ] **If the `<Publish>` that inserts it does not take effect, the failure mode
      is benign**: no prompt, and the install behaves as it did before. Do not
      spend the session on this.

---

## Findings log

Fill this in during the session. One line each, with what was checked and what the
answer was — including the ones that passed, because "we looked and it is fine" is
the part that gets lost.

| # | Check | Result | Follow-up |
| --- | --- | --- | --- |
| | | | |
