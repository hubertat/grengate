# Grenton configuration

Below is grenton system configuration described, for grengate app to work properly.

## modules required

Grengate app is communicating with grenton system with HTTP requests, module needed for this is GATE HTTP grenton module.
It is capable of sending and receiving HTTP requests to and from the grenton system.
Each endpoint needs configuration, we use two endpoints:
1. Read status endpoint: for updating grengate about grenton object status
2. Update state endpoint: for updating grenton objects state

Important is limitation of grenton GATE HTTP module (or maybe of whole grenton system) for processing requests.
It cannot process too large requests and processing multiple objects takes time - sometime too long for successfull cooperation with other systems (like Homekit).

### read script

Read script contains code ran by grenton GATE HTTP module upon receiving a request: read status endpoint.
This code is responsible for parsing the request -> reading grenton object status -> preparing response -> sending response back to grengate app.
Both request and response are json objects.

### update script

Update script contains code ran by grenton GATE HTTP module upon receiving a request: update state endpoint.

## Request and response contract

Field names are exactly those of the Go `ReqObject` (`Clu`, `Id`, `Kind`, `Cmd`, `Source`, `Light`, `Thermo`, `Shutter`, `MotionSensor`).

### Read script (`hb_multi_gate`)

The request is an array of items, the response an array of results for the items that could be resolved. Items with an unknown CLU are left out; grengate treats a missing item as "missing": the device keeps its last good state (or stays not ready), and the item is logged and counted in `missing_objects`.
A thermostat item needs `Source`, the name of the CLU variable holding the measured temperature.

```json
[{"Clu":"CLU_012abcde","Id":"DOU0001","Kind":"Light"},
 {"Clu":"CLU_012abcde","Id":"THE0001","Kind":"Thermo","Source":"temp_var_name"}]
```
```json
[{"Clu":"CLU_012abcde","Id":"DOU0001","Kind":"Light","Light":{"State":true}},
 {"Clu":"CLU_012abcde","Id":"THE0001","Kind":"Thermo",
  "Thermo":{"TempCurrent":21.5,"TempTarget":22,"TempSetpoint":22,"State":1,"Mode":0}}]
```

A thermostat read returns only what grengate uses: `TempCurrent`, `TempTarget`, `TempSetpoint`, `State`, `Mode`.
`TempMin`, `TempMax` and `TempHoliday` are no longer returned.

### Update script (`homebridge`)

**The response has the shape of the request.**

- A single object in gives a single acknowledgement object out.
- An array in gives an array out: one acknowledgement per request item, same order, no holes. An array with one element is answered as an array.
- An acknowledgement is `{Clu, Id, Kind}`, plus `Error` (a string) when that item failed. Unknown CLUs, unknown kinds and bad payloads give an item with `Error`; they are never skipped.
- Every item runs in `pcall`, so one failing item does not stop the others.
- There is no read-back: the response never carries device state. grengate confirms with a normal read.
- An empty or non-JSON body is answered with a single object `{"Error":"..."}`.
  grengate reports every command of such a request as failed (not run).
- `Error` strings raised by the script carry the bare message (`error(msg, 0)`); an error raised by the CLU's `execute()` is passed on as Lua reports it.

Object write:
```json
{"Clu":"CLU_012abcde","Id":"DOU0001","Kind":"Light","Light":{"State":true}}
```
```json
{"Clu":"CLU_012abcde","Id":"DOU0001","Kind":"Light"}
```

Array write with a failing item (unknown CLU) in the middle:
```json
[{"Clu":"CLU_012abcde","Id":"DOU0001","Kind":"Light","Light":{"State":false}},
 {"Clu":"CLU_nope","Id":"DOU0002","Kind":"Light","Light":{"State":true}},
 {"Clu":"CLU_012abcde","Id":"ROL0001","Kind":"Shutter","Cmd":"MOVEUP"}]
```
```json
[{"Clu":"CLU_012abcde","Id":"DOU0001","Kind":"Light"},
 {"Clu":"CLU_nope","Id":"DOU0002","Kind":"Light","Error":"unknown CLU: CLU_nope"},
 {"Clu":"CLU_012abcde","Id":"ROL0001","Kind":"Shutter"}]
```

Payloads per kind: `Light` takes `Light.State` (bool); `Shutter` takes `Cmd` of `MOVEUP`, `MOVEDOWN` or `STOP`;
`Thermo` takes `Thermo.TempSetpoint`, `Thermo.State`, `Thermo.Mode` (numbers).

Thermostat commands may be partial: only the fields present in the item are set. A command with all three behaves as before.
```json
{"Clu":"CLU_012abcde","Id":"THE0001","Kind":"Thermo","Thermo":{"TempSetpoint":21.5}}
{"Clu":"CLU_012abcde","Id":"THE0001","Kind":"Thermo","Thermo":{"State":1,"Mode":0}}
```
A thermostat item with none of the three fields is answered with an `Error`.
All fields are checked before the first one is set, so an item with a bad field changes nothing.

`Cmd` is `SET` for lights and thermostats; the script ignores it for those kinds.

### What grengate makes of a write response

| Response | Result in grengate |
|---|---|
| Acknowledgement without `Error` | Acknowledged |
| Acknowledgement with `Error` | Failed, did not run (only that command) |
| Object `{"Error": ...}` without `Clu`/`Id` | Every command of the request failed, did not run |
| No acknowledgement for a command (array answered with fewer items, or `{}` from the old script) | Outcome uncertain |
| Timeout, non-2xx status, body that is not JSON | Every command of the request uncertain |

After every write, failed or not, grengate reads the device again (confirm read), so HomeKit ends up with the state the CLU has.

### Not verifiable from Go

The scripts cannot run outside the GATE, so these points rest on the manual check below:

- `pcall` is available in the GATE's Lua and catches errors raised by `execute()`.
- Whether `execute()` raises for an object id that does not exist on a known CLU. If it does not, such a command is acknowledged without effect, and only its confirm read (missing item) shows the problem.
  Step 3 of the manual check shows which applies; an unknown CLU always gives a per-item `Error`.

## Rollout order

1. Deploy a grengate binary that accepts both response shapes (stage 2 or later). It works with the old and the new scripts.
2. Deploy the new scripts (`update-script.lua`, `read-script.lua`). They work with that binary and with the stage 5 binary.
   Deploying them while a binary from before stage 2 still runs is possible, but requires manual check 1 below with that binary's own pass criteria.
3. Only then deploy the stage 5 binary, which sends array writes and partial thermostat commands.
   If anything fails, set `LegacyWriteScript` to true to go back to single-object writes; the new script accepts those as well.

## Manual check on the real GATE

1. After deploying the scripts, with the binary running at that time: switch one light, change one thermostat's temperature and its mode, move and stop one shutter.
   - Stage 2 or later: each command takes effect and the log shows an acknowledgement with no error.
   - Before stage 2: each command takes effect and the HomeKit request returns within a round trip, with no hang.
     The log line "Unmarshal data error" after each write is expected with that binary. Any other error, or a request that does not return, is a failure.
2. After deploying the stage 5 binary: run a scene with at least six lights and confirm in the log that they left in batched requests.
3. Send a command for a device id that does not exist on the CLU and confirm the log shows a per-item error while the rest of the batch is applied.
4. If anything fails, set `LegacyWriteScript` to true.
