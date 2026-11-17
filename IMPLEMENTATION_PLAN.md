# grengate optimize-v2: implementation plan

Design reference: `OPTIMIZE_V2_CONCEPT.md`. Section numbers below (for example "concept 3.4") point into it.
This file replaces the earlier optimization plan, which is preserved on branch `dev-optimize-grenton` and in commit `60528be`.

## Ground rules

- Every stage ends with a binary that builds, passes `go build ./...`, `go vet ./...`, `gofmt -l .` (empty) and `go test -race ./...`, and can be deployed on its own.
- Tests are written before the code they cover and assert behaviour seen at the fake GATE or at the HomeKit characteristic, not internals.
- Stages 1 to 4 work with the Lua scripts that are on the GATE today. Only stage 5 needs new scripts.
- Accessory IDs, pairing data and the config file format do not change. New config fields are optional.
- Each work item below is one commit.

## Config fields added by this plan

| Field | Default | Stage | Meaning |
|---|---|---|---|
| `InfluxDB` (`Enabled`, `URL`, `Token`, `Org`, `Bucket`) | disabled | 2 | Optional InfluxDB v2 reporting |
| `LegacyWriteScript` | false | 5 | Write protocol. false: array requests and partial thermostat commands, which need the new script. true: one single-object request per command and three-field thermostat commands, which the old script understands |
| `SetterQueueSize` | 5 | 5 | Commands per write request. A tuning value only: it does not change what a command contains. Ignored when `LegacyWriteScript` is true |
| `SetterFlushMs` | 30 | 5 | Batching window for writes. Ignored when `LegacyWriteScript` is true |
| `ThermoCycleInSeconds` | 60 | 5 | Minimum time between polls of a thermostat |

`FreshInSeconds` stops having an effect in stage 2. It is still accepted so existing config files load.

Binaries from stages 2 to 4 have no protocol switch: they always use the legacy write protocol, because the old script is what is deployed.

---

## Stage 1: Safety net and standalone fixes

**Goal**: A test suite that compiles and a fake GATE to test against, plus the fixes that do not depend on the redesign.

**Work**:

- [ ] `fakegate_test.go`: an `httptest` server standing in for the GATE. It serves a read path and a set path, keeps device state, records every request,
      and can be told to delay, hang until released, fail, omit devices or return invalid payloads. It answers in old-script style (object with read-back payload) or new-script style (concept 3.7).
- [ ] Test helper that builds a `GrentonSet` wired to the fake GATE without a config file.
- [ ] `light_test.go`: fix the `Init` call that does not compile. Rewrite `TestLight_OutletRemoteUpdateQueuesLightRequest` to assert the request the fake GATE received instead of reading the broker's queue.
- [ ] `go vet` findings: three `Logf` calls without format verbs (`gate_broker.go`), unbuffered signal channel (`main.go`).
- [ ] `main.go`: `--version` prints "grengate" instead of "blesrv".
- [ ] `shutter.go`: create `cancelMovement`; refuse a move when `MaxTime` is below 100 ms (log, put the HomeKit target back) instead of sending on the channel.
- [ ] Startup warning for duplicate accessory IDs, naming both devices (concept 3.9).
- [ ] Remove dead code: `Light.Get`, `Thermo.GetTemperature`, `Thermo.GetState`, `CluObject.Update`, `RequestAndUpdate`, `CheckFreshness`, `MotionSensor.SetOn` with its timer fields, the commented block in `SendReq`.
- [ ] `CLAUDE.md`: drop the references to `HkSetupId`, `grengateVer` and freshness checking.

**Success Criteria**:

- `go test -race ./...` compiles and passes.
- A shutter move with `MaxTime` 0 returns at once and leaves the HomeKit target at the current position.
- Two configured devices with the same accessory ID produce one log line naming both; startup continues.

**Tests**:

- Existing light and config tests pass against the fake GATE.
- `TestShutter_MoveRefusedWithoutMaxTime`.
- `TestDuplicateAccessoryIdIsReported`.
- `TestFakeGate_*`: the fake itself answers in both styles and honours hang, fail and omit instructions.

**Status**: Not Started

---

## Stage 2: Lanes, refresh and metrics

**Goal**: `GateBroker` is replaced by a read lane and a write lane (concept 3.1), refresh has a defined completion (concept 3.6), and every request is measured (concept 3.8).
Behaviour towards the GATE stays as today: single-object writes, same scripts. HomeKit callbacks still wait for their result in this stage.

**Work**:

- [ ] `lane.go`: mailbox keyed by device in arrival order, worker taking at most one request's worth, one-slot result channel per submission,
      fan-out to every channel of a merged read entry, "superseded" result for a replaced write, completion stamp from one process-wide counter.
      Two hooks set by `GrentonSet`: one called when a request is sent, one when an entry completes. Both must return without blocking.
- [ ] GATE client used by the lanes: POST, 10 s timeout, decoding of a read response (array) and of a write response in both shapes (object or array, concept 3.7).
      Results distinguish acknowledged, per-item error and uncertain outcome (concept 3.2).
- [ ] `registry.go`: one device map keyed by `Clu` + `Id` (case-insensitive, as the input server matches today) and a small device interface.
      It replaces `FindLight`, `FindThermo`, `FindShutter`, `FindMotionSensor` and the switch in `update()`.
- [ ] `CluObject` gets a mutex guarding the device's fields; `LoadReqObject` and the setters take it. This is what makes `-race` meaningful from here on.
- [ ] Write path on the write lane in the legacy protocol: one command per request, sent as a single object. The callbacks submit and wait for the result. A write response is never loaded as device state;
      the dead `LoadReqObject(obj)` calls after `SendReq` in `thermo.go` go away with `SendReq`.
- [ ] `Refresh` on the read lane: submit every device, wait for an outcome per device (applied, discarded, missing or invalid, failed), count them.
      `main.go` runs one refresh before the HomeKit server starts.
- [ ] Refresh scheduling without `time.Ticker`. A ticker keeps one tick that fired during a slow refresh and delivers it right after, which would start a second refresh at once.
      Instead the cycle boundaries are fixed from the start time; after a refresh completes, the loop waits for the first boundary that is still in the future.
      Boundaries that passed during the refresh are skipped and counted in the log. The next-boundary calculation is a pure function, and the loop takes an injected clock.
- [ ] Autotest on the read lane with validation of identity and payload, covering all device kinds, report-only (concept 3.9). Remove `TestGrentonGate` and `Clu.block`.
- [ ] `metrics.go`: `Metrics` interface, no-op implementation, one debug log line per request. Overlap detection between the two lanes.
- [ ] `influx.go`: InfluxDB v2 line protocol over plain HTTP with the schema of concept 3.8. Buffered, sent in the background, bounded buffer, never blocks a lane.
- [ ] Delete `gate_broker.go`. Update `config.json.example` and `README.md` for `InfluxDB` and the unused `FreshInSeconds`.

**Success Criteria**:

- A submission returns within a millisecond while the fake GATE holds a request open.
- Never more than one request in flight per lane.
- With N devices and `QueryLimit` L, one refresh sends ceil(N / L) read requests and every device is read in every cycle.
- A successful write produces no error log line.
- A refresh completes when a request fails or a device is missing, and reports the counts.
- A refresh that runs past one or more cycle boundaries is never followed immediately by another; the next one starts at the first boundary after it finished.
- With the fake GATE hanging on every request, startup reaches the HomeKit server after the request timeouts and not later.
- Points written to a fake InfluxDB endpoint match the schema, with integer fields carrying the `i` suffix.

**Tests**:

- Lanes: submit during a hanging request; replacement of a waiting command and its "superseded" result; a command already in the request being sent is not replaced;
  results nobody reads do not block the worker; merged reads each get the outcome on their own channel; stamps increase in completion order.
- GATE client: both write response shapes; per-item `Error`; timeout, non-2xx and unreadable body reported as uncertain.
- Refresh: request counts for N below, equal to and above `QueryLimit`; completion with failed requests and missing devices.
- Scheduling: the next-boundary function for a refresh shorter than, equal to and several times longer than the cycle;
  with the injected clock, a refresh that overruns one boundary is followed by a wait until the next boundary, not by an immediate second refresh; skipped boundaries are counted.
- Autotest: passes on a matching item, fails on an empty response, on a wrong identity and on a missing payload.
- Metrics: each event reported once; overlap flag set when the fake GATE holds a read open during a write.
- Influx writer: line format, integer suffix, tag escaping, behaviour when the endpoint is down.

**Status**: Not Started

---

## Stage 3: Device state and non-blocking writes

**Goal**: Lights and thermostats follow the ordering rules (concept 3.3) and readiness (concept 3.5), commands are immutable snapshots, and their HomeKit callbacks return without waiting for the GATE (concept 3.2).
The shutter gets the same state handling for reads but keeps its waiting callback until stage 4.

**Work**:

- [ ] Wire payload types separate from the device structs, with pointer fields so an absent field can be told from zero.
      `ReqObject` carries these instead of `*Light`, `*Thermo`, `*Shutter`, `*MotionSensor`. Validity per kind as in concept 3.5.
- [ ] Per-device state: generation, pending or in-flight command, ready flag. The read lane's send hook records generations; the completion hooks apply rules 2, 4 and 5, including the confirm read.
- [ ] Command snapshots built under the device lock. Thermostat merge in the write mailbox (field-wise); plain replace for lights and shutters.
      The thermostat snapshot carries all three fields in this stage; partial commands arrive with the new protocol in stage 5.
- [ ] Light and thermostat callbacks: apply rule 1, submit, return.
- [ ] Readiness in HomeKit: `ValueRequestFunc` answers reads of a not-ready device with `JsonStatusServiceCommunicationFailure`;
      `SetValueRequestFunc` refuses a write whose baseline is unknown with the same status, logs it and queues a read.
- [ ] Thermostat: sync `TargetHeatingCoolingState` from Grenton state and mode (off; heat for mode 0; auto for mode 1).
- [ ] Input server: a motion sensor push is applied under rule 3; unknown device or bad payload returns a 4xx.
- [ ] A test pinning hap's same-value behaviour (concept 3.5), so a library upgrade that changes it is noticed.

**Success Criteria**:

- A HomeKit request setting ten lights returns in under 50 ms against a fake GATE that takes 300 ms per request.
- Each row of the ordering table in concept 3.3 holds.
- A failed write leaves HomeKit showing the state the fake GATE actually has, after one confirm read.
- A thermostat that was never read refuses a temperature change; nothing is sent.
- A device with an invalid or missing read result reports an error status on HomeKit reads; a ready device keeps its last good values.
- After a restart HomeKit shows the thermostat's real mode.

**Tests**:

- One test per row of the ordering table, each driven by releasing held fake GATE requests in a chosen order.
- Command snapshot: a poll result arriving between `Set(on)` and dispatch does not change the command sent.
- Thermostat merge: a mode change and a setpoint change pending together arrive as one command with both.
- Readiness: refused write with error status; read with error status; invalid results (missing payload, missing field) leave the device not ready and a ready device unchanged.
- hap pin: a same-value write returns status 0 and calls no hook; a different-value write calls the hook.
- Input server: push then older poll result; 4xx on unknown device and on bad payload.

**Manual check before deploying**: one device with a deliberately wrong id shows as "No Response" in the Home app, the rest work.

**Status**: Not Started

---

## Stage 4: Shutter simulation

**Goal**: The shutter simulation follows command results and observed state (concept 3.4), and its HomeKit callback no longer waits for the GATE.

**Work**:

- [ ] Shutter goroutine with one inbox for new targets, command results, read observations and clock ticks. The clock is injected so tests control time.
- [ ] Belief, movement generation and the clock rules. Evidence is applied by completion stamp.
- [ ] Automatic `STOP` at the target, issued by the latest generation only.
- [ ] "Did not run" detection, one retry, and giving up with the HomeKit target set to where the motor ends up.
- [ ] Reconciliation after an uncertain outcome, per the table in concept 3.4.
- [ ] Shutter readiness: a usable `MaxTime` (at least 100 ms). Replaces the stage 1 guard.
- [ ] Remove `moveLoop`, `cancelMovement`, `looping` and `moveTicker`.

**Success Criteria**:

- A new target returns to HomeKit without waiting for the GATE.
- The position does not change before the move is acknowledged.
- A retargeted movement never issues a `STOP`.
- Every row of the failure table and of the reconciliation table in concept 3.4 holds.
- No goroutine is left behind after any test (checked at the end of each shutter test).

**Tests**:

- Clock starts on acknowledgement, not on submit.
- Retarget before the move was sent: one command reaches the fake GATE.
- Retarget while moving: the old target is passed without a `STOP`; the new command's acknowledgement switches the belief.
- `MOVEUP` in flight, `STOP` requested, `MOVEUP` acknowledged, `STOP` fails: the motor is believed moving up.
- Acknowledgement processed after a newer observation: the belief stays at the observation.
- Per-item error on a move, on a retarget and on the automatic `STOP`; retry once; give-up target.
- Uncertain move with a read showing: same direction, other direction, stopped, read failed.
- Uncertain `STOP` with a read showing stopped, and showing still moving.
- Externally observed stop ends the simulation.
- Move refused while `MaxTime` is unknown.

**Status**: Not Started

---

## Stage 5: GATE protocol: scripts, batched writes, read tuning

**Goal**: New Lua scripts on the GATE, writes sent as batched arrays with partial thermostat commands, thermostats polled on a slower cadence, and the remaining tuning done from measurements.

**Work**:

- [ ] `grenton/update-script.lua`: response shaped like the request, acknowledgement items with `Error`, `pcall` per item, thermostat sets only the fields present, no read-back (concept 3.7).
- [ ] `grenton/read-script.lua`: thermostat read trimmed to current temperature, target temperature, setpoint, state and mode.
- [ ] `grenton/README.md`: the request and response contract, and the rollout order.
- [ ] Deploy both scripts and run the manual check below against the binary from stage 4.
- [ ] `LegacyWriteScript` switch. New protocol (default): every write request is an array, also when it holds one command, and thermostat commands are partial.
      Legacy protocol: unchanged from stage 4. The protocol decides what a command contains; `SetterQueueSize` and `SetterFlushMs` only decide how commands are grouped.
- [ ] Write lane batching in the new protocol: up to `SetterQueueSize` commands per request after the `SetterFlushMs` window.
- [ ] `ThermoCycleInSeconds`: thermostats are queued in a refresh only when due; devices that are not ready are queued every cycle; devices not due count as `skipped_objects`.
- [ ] Measure on the real installation and record the results in `README.md`: write request duration against item count, queue wait, lane overlap, shutter position error on partial moves. Adjust `SetterQueueSize`, `SetterFlushMs` and `QueryLimit` defaults from them.
- [ ] Final documentation: `README.md`, `config.json.example`, the architecture section of `CLAUDE.md`. Remove this file.

**Success Criteria**:

- A HomeKit request setting ten lights reaches the fake GATE as two requests of five.
- A temperature change sends a thermostat item with `TempSetpoint` only; a mode change sends `State` and `Mode` only.
- With `SetterQueueSize` 1 in the new protocol, each request is a one-item array and thermostat commands stay partial.
- With `LegacyWriteScript` true the binary sends single objects with three-field thermostat commands, whatever `SetterQueueSize` says.
- With a 10 s cycle and `ThermoCycleInSeconds` 60, a thermostat is read once per six cycles; one that is not ready is read every cycle.
- The manual check passes on the real GATE.

**Tests**:

- Batching: commands submitted inside the window leave in one request; more than `SetterQueueSize` commands split into several requests, oldest first.
- A per-item `Error` in an array response fails only that command; the others are acknowledged.
- Partial thermostat commands, and their merge when both are pending.
- Partial thermostat commands with `SetterQueueSize` 1: a one-item array carrying only the changed field.
- A thermostat that was never read accepts a partial command in the new protocol and still refuses in the legacy protocol.
- Legacy protocol selected by `LegacyWriteScript`, unaffected by `SetterQueueSize`.
- Thermostat cadence, including the not-ready exception and the `skipped_objects` count.

**Manual check on the real GATE** (the Lua scripts cannot be tested from Go):

1. After deploying the scripts, with the binary that is running at that time: switch one light, change one thermostat's temperature and its mode, move and stop one shutter.
   What counts as a pass depends on that binary:
   - Stage 2 or later: each command takes effect, and the log shows an acknowledgement with no error.
   - Before stage 2: each command takes effect and the HomeKit request returns within a round trip, with no hang.
     The log line "Unmarshal data error" after each write is expected here: that binary reports every successful write this way, with the old script as well. Any other error, or a request that does not return, is a failure.
2. After deploying the stage 5 binary: run a scene with at least six lights and confirm in the log that they left in batched requests.
3. Send a command for a device id that does not exist on the CLU and confirm the log shows a per-item error while the rest of the batch is applied.
4. If anything fails, set `LegacyWriteScript` to true to return to single-object writes. The new script accepts those as well.

**Status**: Not Started

---

## Deployment order

| After stage | Deployable | Needs on the GATE |
|---|---|---|
| 1 | Yes | Current scripts |
| 2 | Yes | Current scripts |
| 3 | Yes | Current scripts |
| 4 | Yes | Current scripts |
| 5 | Scripts first, then the binary | New scripts |

The new scripts stay compatible with every binary from stage 2 on, so they may be deployed earlier than stage 5 if that is convenient.
Deploying them while a binary from before stage 2 is still running requires step 1 of the manual check with that binary and its own pass criteria, because that pairing cannot be tested from Go.

## Risks and unknowns

- **GATE behaviour on a Lua error.** Unknown whether it answers with an error status or lets the request time out. With the current write script every thermostat write ends in a Lua error after the set is applied,
  so until the new script is deployed a thermostat write is reported as uncertain and may hold the write lane for up to the 10 s timeout, as it holds the setter today.
- **Reads and writes at the same time.** The two lanes can overlap, as the two brokers do today. The overlap metric from stage 2 shows whether writes suffer; the remedy is a smaller `QueryLimit`.
- **Confirm reads assume a write has taken effect when the GATE answers.** If a CLU applies a command with a delay, a confirm read can show the old state for one cycle. The stage 3 manual run will show it.
- **Stricter validity.** A thermostat whose sensor variable is missing returns no current temperature today and shows 0 degrees. From stage 3 it shows as not responding. This is intended, but it is a visible change.
- **Startup time when the GATE is down.** The startup refresh waits for its requests to time out before the HomeKit server starts: about 10 s per read request, so about 20 s for 60 devices with `QueryLimit` 30.
- **"No Response" presentation.** That a read error status shows as "No Response" in the Home app is the expected HAP behaviour but is not covered by a Go test; the stage 3 manual check covers it.
- **Shutter accuracy.** Not bounded by the design (concept 3.4). Measured in stage 5.
