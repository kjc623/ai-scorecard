# Waiting on the owner

Everything the backlog has left for the owner to run, check or answer, in one list.

Agents add lines and never tick or remove them. The owner ticks a line when it is done. An agent
whose task relies on an unticked line does not assume it happened: check, and if it has not, say so.

Each line names the task that left it. The exact commands are in that task's `REPORT.md`.

## Run

- [ ] **Run task 00** before task 05, on a clean tree at the head of `main`. It expects
  `git status` to show nothing but its own changes. (review)
- [ ] **Rebuild and reinstall the lab MSI on the Windows host**, from the head of the stack, with
  the lab up: `node installer/lab-msi.mjs`, then `msiexec /i installer\dist\ShadowAICapture.msi`.
  One reinstall covers both tasks that asked for it. On 2026-10-04 the real device's row on Devices
  still showed no hostname, agent version or mode, so this had not been done. (02, 04)
- [ ] **Apply the schema changes to any database other than the lab's.** The lab's database has
  had all three applied by the agents. Anything else needs 02's four statements (in its report),
  then `backlog/03-findings/MIGRATION.sql`, then `backlog/04-device-identity/MIGRATION.sql`, in that
  order. (02, 03, 04)

## Verify

On the owner's dashboard, `http://127.0.0.1:8787`, with the real device.

- [ ] After sending prompts from the device: Tools shows tools with submission and people counts
  and a time chart; Users shows a series for `lab-user`; Data classes shows the classes seen; no
  "no_watermark_row" banner. (01)
- [ ] The device shows "Reporting" with a recent last-seen time, and the two cards on Devices
  agree. (02)
- [ ] After the reinstall: the device sends its heartbeat, and its collectors appear as observed
  in the coverage banner. (02)
- [ ] A prompt containing a test card number produces a finding in Search > Findings and on the
  Overview; opening it shows the event; "Open findings" counts it. Task 03 showed this with a
  direct database call, not with the device. (03)
- [ ] After the reinstall: the device appears by hostname on Devices and in search results, with
  its agent version and mode, and the user shown is the person who typed the prompt. (04)

## Answer

- [ ] **Which mode does the Devices "Mode" column show?** It was built on the agent's
  recommendation (the device's effective base mode) because the question was left open. Task 12
  displays the same value. (04)
- [ ] **Go through `FOLLOWUPS.md`.** Seven rows say a remaining brief is now wrong, in 06, 11, 12
  and 13; edit the brief or strike the row. One deferred item blocks task 12: nothing writes the
  signed policy bundle, and no task builds it. (review)
- [ ] **Remove the simulated devices from the owner's tenant, and restore the device task 02
  backdated?** Eight of the ten enrolled devices there are simulated. Simulated data now goes to
  the sample tenant, but what is already there stays until it is deleted. (01, 02, 04)
