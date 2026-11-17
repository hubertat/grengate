# grengate optimize-v2: concept

Status: sixth revision, all decisions made (section 7). This is the concept, not the implementation plan.
It replaces the approach of the earlier optimization plan (executed on `dev-optimize-grenton`, where that plan is preserved).
The implementation plan for this concept is `IMPLEMENTATION_PLAN.md`.

## 1. Context

- About 60 devices on 4 CLUs: mostly lights, about 6 thermostats, some shutters and motion sensors.
- The GATE has separate HTTP objects and Lua scripts for read (`hb_multi_gate`) and write (`homebridge`).
- Whether the GATE handles a read and a write at the same time is unknown.
- State changes made in Grenton (wall switches, scenes, schedules) must reach HomeKit.
- Polling is the mechanism that brings Grenton-side changes to HomeKit. The existing push for motion sensors stays as it is; push is not extended to other devices.
- InfluxDB v2 reporting stays, optional.

## 2. What is actually slow or wrong today

The old plan treated queue internals (busy-wait, O(n) scans, lock scope) as the bottleneck.
They are not: a GATE round trip costs 100 ms or more, the queue code costs microseconds.
The real problems are these.

| Problem | Effect |
|---|---|
| hap runs all writes of one HomeKit request in a sequential loop, and every `Set()` blocks until its HTTP round trip finishes | A scene with N devices takes N round trips, one after another |
| Read flush timer condition is inverted (`gate_broker.go:88`) | Depending on the exact device count, devices are polled every one to two cycles instead of every cycle; leftovers wait for the next cycle |
| Write success never signals the caller (`gate_broker.go:162`) | `Set()` returns only because the response fails to parse; every good write logs an error |
| `Shutter.cancelMovement` is never created (`shutter.go:130`) | Retargeting a moving shutter, or moving one before `MaxTime` is known, hangs the HomeKit request forever |
| `Flush` clears the queue after releasing its lock (`gate_broker.go:107`) | Overlapping flushes can resend a batch and drop newer requests |
| Write script reads back state after each set, with a wrong field name for thermostats (`update-script.lua:101`) | Extra `execute()` calls per write; thermostat writes end in a Lua error after the set is applied |
| Every thermostat write sends setpoint, state and mode from grengate's local copy (`thermo.go:124-162`) | A temperature change before the first poll switches the thermostat off (state is still 0); any write can overwrite a change made on a wall panel since the last poll |
| A poll that started before a write can land after it | HomeKit state flips back, then corrects on the next cycle |
| hap answers a write equal to its cached value with success and never calls grengate (`characteristic/c.go:155`) | A command is lost whenever the cached state is stale or made up |
| `go test ./...` does not compile (`light_test.go:17`) | No safety net for any of the changes below |

Estimated GATE load per poll cycle, assuming mostly lights: about 100 `execute()` calls,
nearly half of them from the six thermostats (8 calls each, against 1 per light).

## 3. Design

### 3.1 One lane per GATE endpoint

Replace `GateBroker` (queue slice, two mutexes, sleep loop, error channels) with a lane per endpoint: a read lane and a write lane.
Each lane has two parts.

- **Mailbox.** Pending requests keyed by device (`Clu` + `Id`), in arrival order, protected by a mutex that is held only for the mailbox operation, never during HTTP.
  Submitting means: lock, add the entry or replace the existing one in place, unlock, then a non-blocking send on a one-slot wake-up channel.
  Submission therefore takes microseconds whatever the lane is doing, including during a 10 s GATE timeout.
  The mailbox cannot grow beyond one entry per device, so there is no "queue full" state, no rejection and no retry loop.
- **Worker.** One goroutine that waits for the wake-up, then repeats: take the oldest entries from the mailbox, at most one request's worth, release the lock, do the HTTP call.
  Everything it did not take stays in the mailbox, where a newer request can still replace it.
  A command is out of reach of replacement only once it is part of the request being sent.
  Each lane sends strictly one request at a time.
- **Results.** Every submission gets exactly one result. Delivering it never blocks the worker or the submitter:
  each submission has its own one-slot result channel that is written once. A caller that never reads it costs nothing.
  A read submitted for a device that is already in the mailbox is merged into that entry, and the entry keeps the result channel of every submission merged into it.
  When the entry completes, the same outcome is written to each of those channels. One channel is never shared between callers.
- **Completion stamp.** When a response completes, or a request is given up as failed, the worker takes the next number from one process-wide counter shared by both lanes.
  Every result and every read observation from that response carries the stamp. It orders evidence that reaches a device by different paths (3.4).

Request size: `QueryLimit` for reads, as today. Writes get their own limit (`SetterQueueSize`, the name used on the old branch), starting conservatively at 5 until measured.

Two independent lanes is what runs in production today, against separate GATE HTTP objects, so this keeps current behaviour.
The metrics in 3.8 include queue wait and read/write overlap so contention between the lanes can be seen.
If writes get slower while a read is in flight, the fix is a smaller read request size.

### 3.2 Write path

- HomeKit callbacks stop waiting for the GATE: update local state, submit a command, return.
- **Commands are immutable snapshots.** A command is a small dedicated value built under the device lock at submit time. It never points at the live device,
  so a poll or a later callback cannot change what a queued command means. Today `Light.Set` and the thermostat setters put a pointer to the device into the request.
- **Commands carry only what the user changed.** Light `{State}`. Shutter `Cmd`. Thermostat: a temperature change carries `{TempSetpoint}`, a mode change carries `{State, Mode}`.
  The field names are the ones the Lua script already reads. Partial thermostat commands need the new script (3.7); until it is deployed the thermostat sends all three fields as today, behind the readiness check in 3.5.
- **Latest desired state wins per device.** A newer command replaces the pending one for the same device.
  For lights and shutters that is a plain replace. For thermostats it is a field-wise merge:
  a mode change and a setpoint change that are both still pending become one command carrying both.
- **Command results:** acknowledged, failed with a per-item error, failed with an uncertain outcome, or superseded by a newer command.
  "Uncertain" covers timeouts, transport errors, non-2xx responses and unreadable responses: the command may or may not have run. Most callers treat both failures alike; the shutter does not (3.4).
- The write worker waits a short window after the first wake-up, then starts sending.
  A scene's callbacks all arrive within about a millisecond once they no longer block, so 20 to 50 ms is enough. Configurable (`SetterFlushMs`).
- The write response is an acknowledgement only (3.7). It is never loaded as device state.
- After a write finishes, successfully or not, the affected devices get a confirm read (rule 5 in 3.3).

### 3.3 Per-device ordering

A single "last written at" timestamp is not enough: it misses writes that are still queued, and it says nothing about push updates from motion sensors.
Each device instead keeps, under its own mutex:

- the observed state (what HomeKit shows),
- whether it has a command pending or in flight,
- a generation counter.

Rules:

1. **HomeKit write.** Bump the generation, set the optimistic state, submit the command snapshot. The device now has a command pending.
2. **Write finished** (acknowledged or failed). Bump the generation. If no newer command is pending, the device is no longer busy.
3. **Push carrying state** (motion sensors only, 3.6). Applied, and it bumps the generation. Motion sensors take no commands, so nothing can be pending.
4. **Read result.** When a read request is sent, the generation of each device in it is recorded.
   A result is applied only if that device's generation is unchanged and no command is pending or in flight. Otherwise it is discarded and counted.
5. **Confirm read.** Queued when a write finishes, only for devices without a newer pending command. It is an ordinary read, so rule 4 applies to it.

What this covers:

| Case | Outcome |
|---|---|
| Write queued behind another batch, a poll starts afterwards and returns the old state | Discarded: a command is pending (rule 4) |
| Poll sent before a write, result arrives after it | Discarded: generation changed (rules 1, 2, 4) |
| Older poll result arrives after a newer push | Discarded: the push bumped the generation (rules 3, 4) |
| Confirm read of an earlier write returns after a newer command was queued | Discarded: generation changed and a command is pending (rules 1, 4) |
| Write fails | Confirm read puts HomeKit back to the real state (rules 2, 5) |

### 3.4 Shutter simulation

Today the position clock starts after `sendCmd` returns, which is after the GATE round trip.
With immediate submission that would start the clock before the shutter moves, so the simulation has to follow command results and observed state instead.
The shutter's own goroutine keeps two things apart.

**Evidence: what the motor is doing.** The goroutine holds a belief: stopped, moving up or moving down. It changes only on evidence:

- the acknowledgement of any command, whichever movement generation it belongs to. An acknowledged move means the motor is moving in that direction, an acknowledged `STOP` means it stopped.
  The write lane sends one request at a time, so acknowledgements complete in the order the commands were sent;
- the state shown by a read that passes the ordering rules.

A failed or superseded command is evidence of nothing and leaves the belief as it is.

**Evidence is applied in the order its GATE response completed,** not in the order the goroutine happens to get to it.
Acknowledgements arrive through result channels and observations through the read path, so the two can reach the goroutine in either order. Movement generation cannot tell them apart.

- Each piece of evidence carries the completion stamp of its response (3.1).
- The shutter remembers the stamp of the last evidence applied to its belief. Evidence with a lower stamp does not change the belief.
- A late acknowledgement still records that its command ran, so the command is not retried. It only loses the right to override newer evidence.

Example: `MOVEUP` succeeds and its acknowledgement waits in the result channel. The confirm read observes the motor stopped and is processed first.
When the goroutine then takes the acknowledgement, its stamp is lower than the observation's, so the belief stays "stopped" instead of switching back to "moving up".

The stamps order correctly across the two lanes because of rule 4 in 3.3: a read sent before a write finished is discarded for that device.
Any observation that reaches the shutter therefore comes from a request sent after every finished write, and completes after them.

**Authority: which target counts.** Every new target from HomeKit starts a new movement generation. The latest generation alone holds the target:

- only it may submit the automatic `STOP` at its target;
- only its commands are retried;
- an older target is never restored.

**Clock.** It runs while the motor is believed moving, in that direction.
If that movement belongs to the latest generation it runs towards the target. Otherwise it has no target and is limited only by the end stop.

Normal operation:

- The HomeKit callback records the target, starts a new generation and wakes the goroutine.
- From standstill: submit the move. On acknowledgement the motor is believed moving and the clock starts.
- At the target, latest generation only: submit `STOP`. On acknowledgement the motor is believed stopped.
  Because the `STOP` is submitted only after the move was acknowledged, it can never replace its own unsent move.
- Retarget before the move was sent: the newer command replaces it in the mailbox (3.1). Superseded is not evidence, so nothing else happens.
- Retarget while a command is in flight or the motor is moving: the new command (other direction or `STOP`) is submitted under the new generation.
  The old generation can no longer issue a `STOP`, even if its simulated target is reached meanwhile. Its acknowledgement still counts as evidence.
  Example: `MOVEUP` is in flight, the user asks for `STOP`, `MOVEUP` is acknowledged. The motor is now believed moving up and the clock runs upwards without a target. The result of the `STOP` decides what happens next.
- Externally observed stop: a read shows the shutter stopped; the simulation ends, the position is kept and the target set to it.
- `MaxTime` unknown: the move is refused (3.5).
- Unchanged: movement started from a wall switch updates the HomeKit movement state but not the position, as today.

When a command of the latest generation did not run:

- A move did not run if it got a per-item error, or its outcome was uncertain and a read shows the motor moving the other way.
- A `STOP` did not run if it got a per-item error, or its outcome was uncertain and a read shows the motor still moving.
- Such a command is retried once. Its generation keeps the authority meanwhile.
- If the retry does not run either, grengate gives up on that target: it logs the failure and sets the HomeKit target to where the motor is believed to end up,
  which is the current position if stopped, or the end stop of its direction if moving.

Failures:

| Result | Belief | What else happens |
|---|---|---|
| Per-item error, latest generation | Unchanged | Retried once, then given up as above |
| Per-item error, older generation | Unchanged | Nothing |
| Uncertain outcome, any generation | Unchanged; the clock carries on accordingly | The confirm read decides, see below |

Reconciliation after an uncertain outcome of a latest-generation command. In every row the belief is set to what the read shows:

| Uncertain command | Read shows | Conclusion |
|---|---|---|
| Move | Moving in its direction | It ran. The clock runs towards the target from now |
| Move | Moving the other way | It did not run. Retried once, then given up |
| Move | Stopped | Ambiguous: it did not run, or it ran and the movement already ended. Simulation ends, target set to the position. Not retried |
| `STOP` | Stopped | Simulation ends, target set to the position |
| `STOP` | Still moving, either direction | It did not run. The observed movement is kept, the `STOP` is retried once under the same generation, whose target (the position where the stop was due) keeps the authority. If the retry does not run either: given up, target set to the end stop of the observed direction |
| Any | The read fails as well | Stays uncertain. The next poll tries again |

Accuracy. Acknowledgement-driven simulation is an approximation, and its error is not bounded by the design:

- The motor starts during Lua execution, but the acknowledgement arrives after the whole write request finishes. The gap grows with the number of items in the request.
- The delay before a `STOP` takes effect is queue wait plus the batching window plus the round trip. Under backlog it grows.
- Either can exceed one position step (1 % of `MaxTime`). The error adds up over partial moves and is reset by a full open or close.

No accuracy figure is claimed until this is measured with representative batches. The metrics in 3.8 (write request duration against item count, queue wait) give the numbers.

### 3.5 Device readiness

A startup refresh (3.6) normally loads every device, but a read can fail or leave out individual devices, so readiness is tracked per device:
a device is ready once one valid read result for it has been applied. Readiness is not revoked later; detecting a GATE outage after startup is out of scope here.

A read result is valid only if it carries the payload for the device's kind with every field grengate uses.
The response is decoded so that an absent field can be told from a zero value; today a field the Lua script left out silently becomes 0 or `false`.

| Device | Valid read result |
|---|---|
| Light, outlet, motion sensor | Payload with `State` |
| Shutter | Payload with `State` and a usable `MaxTime`: at least 100 ms, so that one position step is at least 1 ms |
| Thermostat | Payload with current temperature, target temperature, setpoint, state and mode |

An invalid result is never applied, for a ready device either: the device keeps its last good values, and the result is logged and counted (3.6).

What a write needs, and what happens to a write that differs from the value hap has cached:

| Device | Needs | Not ready |
|---|---|---|
| Light, outlet | Nothing: the command is absolute | Write goes through |
| Shutter | `MaxTime` | Move refused |
| Thermostat, partial commands (new script) | Nothing: only the changed field is sent | Write goes through |
| Thermostat, three-field commands (old script) | Setpoint, state and mode | Write refused |

A refused write returns an error status to HomeKit through hap's `SetValueRequestFunc`, is logged, and queues a read of that device.
A device that is not ready also answers HomeKit reads with an error status, so it shows as not responding instead of showing made-up defaults such as "off" or "fully open".

**Limit set by hap.** hap compares an incoming write with its cached value before it calls any grengate code, and answers success without calling it when the two are equal.
This is the same in the pinned v0.0.20 (`characteristic/c.go:155`) and in the latest v0.0.35; the switch that disables the comparison is not exported. Consequences:

- grengate can refuse only a write that differs from the cached value. A write equal to it is reported to HomeKit as success and never reaches grengate, ready or not.
- For a device that is not ready the cached value is a made-up default. "Off" sent to a light that is really on is lost; a same-value write to an unready thermostat reports success instead of a refusal.
- Nothing wrong is ever sent to Grenton because of this, since a dropped write sends nothing. The gap is a false success.

How this is handled:

- The not-ready window is kept short. The HomeKit server starts only after the startup refresh has finished, whatever its outcome, and devices that are not ready are read on every cycle regardless of their cadence.
- The cache is kept close to the truth by confirm reads and by polling that actually runs every cycle. For a change made in Grenton the window in which a matching command is lost is at most one poll cycle.
- Closing the gap completely needs a patched hap (one exported setter, carried through a `replace` in `go.mod` or proposed upstream). That is not part of this work: the limit is accepted (section 7).

### 3.6 Read path and polling

- One refresh runs at startup before the HomeKit server starts. Today the first data arrives one to two cycles after start. Devices it did not reach stay not ready (3.5).
- Polling continues with `CycleInSeconds`. The inverted timer goes away with the old broker: each cycle sends everything it queued.
- **A refresh is complete when every device it queued has an outcome,** not when every result has been applied. The outcomes are:
  applied (changed or unchanged), discarded by the ordering rules, missing from the response or invalid (3.5), or failed with its request.
  Each request is bounded by the HTTP timeout, so a refresh always completes. Missing, invalid and failed devices are logged and counted (3.8).
- Only one refresh runs at a time. Cycle boundaries are fixed from the start time; after a refresh completes, the next one starts at the first boundary still in the future.
  Boundaries that passed during a refresh are skipped. A `time.Ticker` is not used for this, because it keeps one tick that fired during a slow refresh and delivers it right after.
- Thermostats are polled less often than lights (for example every 6th cycle), since temperature moves slowly and they cost 8 calls each.
- The read script returns only what grengate uses for a thermostat: current temperature, target temperature, setpoint, state, mode.
  `TempMin`, `TempMax` and `TempHoliday` are read today and never used.
- I would not build the old plan's "skip stable devices" stage. A device that has been stable for an hour is exactly the one that changes next,
  and stale state costs commands, not only display accuracy (3.5).

Push stays limited to what exists: motion sensors on the input server (`POST /update`). It is not extended to lights, shutters or thermostats.
Two small changes to it:

- A motion sensor push is applied under rule 3 in 3.3, so an older poll result can no longer overwrite it.
- An unknown device or a bad payload returns a 4xx instead of an empty 200.

### 3.7 Lua scripts and the response contract

`update-script.lua`:

- **The response has the shape of the request.** A single object in gives a single acknowledgement object out; an array in gives an array out, one item per request item, in the same order, with no holes.
- An acknowledgement is `{Clu, Id, Kind}` plus `Error` when the item failed. Unknown CLUs get an item with `Error` instead of being skipped.
- Each item runs in `pcall`; a failing item does not stop the rest.
- A thermostat item sets only the fields present in it. A command with all three fields behaves as today.
- No read-back after set.

Why the shape rule matters: the current binary sends single objects and returns from `Set()` only because the object response fails to parse as an array.
If the new script answered a single-object call with an array, parsing would succeed, nothing would signal completion, and every HomeKit write would hang forever.
With "object in, object out" the current binary sees the same shape as today.

Rollout:

1. The first new binary (lanes, still single-object writes with three-field thermostat commands) accepts both response shapes and signals completion properly. It works with the old and the new script.
2. The new script is deployed. It works with the current binary and the first new binary.
3. Only then does the binary switch to array writes and partial thermostat commands.
   The protocol is a switch of its own (`LegacyWriteScript`), separate from the batch size, so tuning `SetterQueueSize` down to 1 never brings three-field thermostat commands back.

Verification: a Go test with the fake GATE covers the new binary against both shapes.
The current binary against the new script cannot be tested from Go, so it is an explicit manual check on the real GATE if the script goes out before the binary:
one light, one thermostat and one shutter command, each must take effect and return within a round trip. The current binary's parse error in the log is expected in that pairing.

`read-script.lua`: trimmed thermostat read as in 3.6. Nothing else changes.

Skipped from the old plan: Lua-side caching, `string.format`, dispatch tables. No measurable gain.

### 3.8 Metrics and InfluxDB v2

- A small `Metrics` interface with a no-op default, called from one place per event.
- InfluxDB v2 implementation behind the existing `InfluxDB` config block, off by default.
- Line protocol is written straight to `/api/v2/write` over plain HTTP, buffered and sent in the background, without `influxdb-client-go`.
- One debug log line per batch (count, bytes, ms). Full JSON bodies only with `Verbose`.

Retained schema. On the old branch `flush`, `command_endtoend` and `refresh_cycle` are values of the `operation` tag, not measurement names.
All fields are integers and must stay integers: a hand-written line must carry the `i` suffix, or InfluxDB rejects it as a type conflict in an existing bucket.

| Measurement | Tags | Fields | Meaning |
|---|---|---|---|
| `grengate_command` | `operation=flush`, `component=gate_broker`; `clu_id`, `object_id` only for a single-object batch | `duration_ms`, `object_count`, `clu_count`, `request_bytes`, `success` (0/1) | One write request. `duration_ms` is the HTTP time. Unchanged |
| `grengate_command` | `operation=command_endtoend`, `component=command`, `clu_id`, `object_id` | `duration_ms`, `queue_wait_ms`, `http_ms` | One command. Meaning changes, see below |
| `grengate_refresh` | `operation=flush`, `component=gate_broker` | same as the write flush | One read request. Unchanged |
| `grengate_refresh` | `operation=refresh_cycle`, `component=grenton_set` | `duration_ms`, `object_count`, `changed_objects`, `skipped_objects` | One poll cycle. Meaning changes, see below |

Changes in meaning:

- `command_endtoend` used to time how long the HomeKit callback was blocked. Callbacks now return at once, so it is measured in the write lane:
  `queue_wait_ms` is submit to request sent, `http_ms` is the request, `duration_ms` is submit to acknowledgement. Superseded commands emit no point.
  On the old branch this point stopped being written once the setters went async, so this brings it back.
- `refresh_cycle.duration_ms` used to cover only queueing. It becomes cycle start to completion as defined in 3.6.
- `changed_objects` and `skipped_objects` were always 0 on the old branch. They become real: devices whose state changed, and devices not due this cycle (thermostats on a slower cadence).

Additions, as new fields on existing points, which does not disturb existing queries:

- Both `flush` points: `queue_wait_ms` (oldest request in the batch, submit to request sent) and `overlap` (1 if the other lane had a request in flight at any time during this one). These two are what answers the lane contention question.
- `refresh_cycle`: `discarded_objects` (dropped by the ordering rules), `missing_objects` (not in the response, or invalid), `failed_objects` (their request failed).
  Together with the applied ones they add up to `object_count`.

Not carried over: `grengate_telemetry` (`operation=queue`). It is defined on the old branch but never written, and "rejected" no longer exists.

### 3.9 Other device code

- One device registry (map keyed by `Clu` + `Id`) and a small interface (`LoadReqObject`) replace the four `Find*` functions and the switch in `update()`.
  This is a simplification, not a performance item.
- Thermostat: sync `TargetHeatingCoolingState` from Grenton state and mode, so HomeKit shows the real mode after a restart.
- Startup check for duplicate accessory IDs. The ID is built from the CLU number and the device number only: the device kind is not part of it,
  and a decimal CLU id loses its first digit (section 4). Two devices can therefore end up with the same ID. grengate logs an error naming both devices and keeps running, so a configuration that runs today still starts.
- Autotest. Today it posts a single read-shaped object to the set endpoint and checks only the HTTP status.
  Pointing it at the read endpoint alone would make it pass without reading anything: the read script loops over an array, so a single object yields an empty response with status 200.
  The autotest therefore sends an array through the read lane and passes a device only if the response contains an item with the same `Clu`, `Id` and `Kind` and the payload for that kind.
  It reports failures and does not remove devices (the current "removal" is a no-op).

## 4. What does not change

- Config file format. New fields are optional with defaults matching today's behaviour where possible.
- Accessory IDs. `GetIntId` drops the first digit of decimal CLU ids (`clu.go:33`); fixing it would change IDs and break room assignments, so it stays.
- HomeKit pairing data and the hap version.
- Separate GATE HTTP objects for read and write.

## 5. Cleanup and tests

- Fix the test build first, then add a fake GATE (`httptest`). Tests cover:
  - lanes: submission while a request hangs; a newer command replacing one that is waiting behind an in-flight request; results that nobody reads do not block the worker;
    several reads merged into one entry each receiving the outcome on their own channel;
  - batching window, latest-wins, thermostat merge, command snapshots;
  - each row of the ordering table in 3.3;
  - both response shapes;
  - shutter: clock start on acknowledgement; a retargeted movement issuing no `STOP`; an older generation's acknowledgement updating the belief (the `MOVEUP`, `STOP`, failed `STOP` sequence);
    each row of the failure and reconciliation tables in 3.4, including an uncertain `STOP` with the motor still moving;
    an acknowledgement processed after a newer observation (`MOVEUP` acknowledged, a read shows stopped, the read is processed first) leaving the belief at "stopped";
  - readiness: refused writes, partial thermostat commands on a device that was never read, and a test that pins hap's same-value behaviour so a library upgrade that changes it is noticed;
    results with a missing payload or missing fields, and a shutter result with `MaxTime` of 0 or below 100 ms, leaving the device not ready and a ready device unchanged;
  - refresh completion with failed requests, missing devices and discarded results; cycle boundaries skipped while a refresh is incomplete, with no immediate second refresh afterwards;
  - autotest validation.
- Remove dead code: `Light.Get`, `Thermo.GetTemperature`/`GetState`, `Update`, `RequestAndUpdate`, `CheckFreshness`, `MotionSensor.SetOn`, `Clu.block`, the commented block in `SendReq`.
- `go vet` findings: three `Logf` calls without format verbs, unbuffered signal channel.
- `--version` prints "blesrv".
- `CLAUDE.md` refers to `HkSetupId`, `grengateVer` and freshness checking that no longer exist.

## 6. Order of work

The stages, their tests and the deployment order are in `IMPLEMENTATION_PLAN.md`:

1. Safety net and standalone fixes.
2. Lanes, refresh and metrics.
3. Device state and non-blocking writes.
4. Shutter simulation.
5. GATE protocol: new scripts, batched writes, read tuning and measurement.

## 7. Decisions

All open points are decided.

- **Polling only.** Grenton-side changes reach HomeKit through polling. Push is not extended beyond the existing motion sensor endpoint.
- **InfluxDB.** Optional InfluxDB v2 reporting over plain HTTP line protocol, without the client library.
- **Thermostat cadence.** A delay of about 60 s on thermostat readings is fine.
- **Dead code.** The items listed in section 5 are deleted.
- **Dashboards.** None exist, so the changes in meaning in 3.8 need no migration.
- **Not-ready devices.** They show as not responding in HomeKit instead of a default state (3.5).
- **HomeKit IDs.** Current accessory IDs are kept so existing HomeKit configuration is not lost. Both hex and decimal CLU ids are in use; the id parsing stays untouched (section 4), and a startup check reports duplicate accessory IDs (3.9).
- **Same-value writes dropped by hap.** The limit described in 3.5 is accepted. A patched hap remains possible later without redesign.
