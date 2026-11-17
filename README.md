# grengate

Apple HomeKit gateway for Grenton home automation systems, written in GO.

Based on great [https://github.com/brutella/hc](https://github.com/brutella/hc) framework.


## what it does

It is a gateway between Grenton home automation system and HomeKit.
In other words: it makes possible to controll Grenton devices in Apple HomeKit system.

Of course other things are needed:

## what is needed

0. Device with *grengate* program running
1. Grenton system
2. Grenton GATE module (HTTP Gate)
3. Lua script running on Grenton Gate module (provided here, read below)

+ some configuration

## configuration

### grengate

#### linux service

Creating user for the service:
```
sudo useradd -r -s /bin/false grengate
```

Example config for linux/unix service using systemd (to put into `/etc/systemd/system`):

```
[Unit]
Description=Grengate
After=network.target
StartLimitIntervalSec=0

[Service]
Type=simple
Restart=always
RestartSec=5
User=grengate
WorkingDirectory=/srv/grengate
ExecStart=/srv/grengate/grengate -config /srv/grengate/config.json

[Install]
WantedBy=multi-user.target
```

Enabling service, so it will run on system startup:
```
sudo systemctl enable grengate
```

#### power outlets

Any Grenton DOUT configured under `Lights` can be shown in HomeKit as a power outlet instead of a lightbulb.
Set `"HkType": "Outlet"` on that entry (default is `"Lightbulb"`):

```json
"Lights": [
	{ "Id": 5678, "Kind": "DOU", "Name": "Desk socket", "HkType": "Outlet" }
]
```

Grenton side (GATE Lua script) is unchanged: it is still read and set as a `Light`.
Grenton has no power sensing, so HomeKit "Outlet In Use" mirrors the on/off state.
Changing `HkType` of an already paired device keeps its accessory id; if the Home app shows it
incorrectly, remove the accessory and let it be re-added.

#### polling and refresh

All devices are read from the GATE every `CycleInSeconds` (default 10), at most `QueryLimit` devices (default 30) per read request.
The first read happens at startup, before HomeKit is started. A refresh that takes longer than a cycle is not followed by an
immediate second one: the next refresh starts at the next cycle boundary, and the skipped cycles are logged.

Thermostats are read less often: at most every `ThermoCycleInSeconds` (default 60, so every sixth cycle with the default
`CycleInSeconds`), rounded to whole cycles. A thermostat that has not been read successfully yet is read in every cycle.
With `ThermoCycleInSeconds` at or below `CycleInSeconds` thermostats are read in every cycle.

`FreshInSeconds` is no longer used. It is still accepted, so older config files load unchanged.

#### writes

HomeKit changes return at once; the commands are sent to the GATE in the background, one request at a time, and the newest
command per device wins while it waits. After every write grengate reads the device again, so HomeKit ends up with the real state.

| Field | Default | Meaning |
|---|---|---|
| `LegacyWriteScript` | `false` | `false`: array requests, thermostat commands carry only the changed value (temperature, or state and mode). Needs the new `grenton/update-script.lua`. `true`: one single-object request per command with all three thermostat values, as the old script needs |
| `SetterQueueSize` | `5` | Commands per write request. Ignored when `LegacyWriteScript` is `true` |
| `SetterFlushMs` | `30` | Batching window: after the first command the write lane waits this long, so a scene leaves in as few requests as possible. Ignored when `LegacyWriteScript` is `true` |

`SetterQueueSize` and `SetterFlushMs` only decide how commands are grouped; what a command contains depends on `LegacyWriteScript` alone.
With `LegacyWriteScript` set to `false` a thermostat accepts changes before its first successful read (only the changed value is sent);
with `true` such a change is refused (HomeKit shows an error) until the thermostat was read.

**Rollout order** (details in [grenton/README.md](grenton/README.md)):

1. Deploy the new scripts (`grenton/update-script.lua`, `grenton/read-script.lua`) on the GATE first, while the previous grengate binary is still running.
   A binary from before the lanes (stage 2 of the plan) works with them too, but needs step 1 of the manual check with its own pass criteria.
2. Then deploy this binary. It uses the new protocol by default.
3. If writes fail with the new protocol, set `"LegacyWriteScript": true` and restart: single-object writes work with the new script as well.

#### metrics (InfluxDB v2)

Optional. Add an `InfluxDB` block to send metrics to an InfluxDB v2 bucket (line protocol over HTTP, token authentication):

```json
"InfluxDB": {
	"Enabled": true,
	"URL": "http://localhost:8086",
	"Token": "your-token",
	"Org": "your-org",
	"Bucket": "grengate"
}
```

Points (all fields are integers):

| Measurement | Tags | Fields | One point per |
|---|---|---|---|
| `grengate_command` | `operation=flush`, `component=gate_broker` (+ `clu_id`, `object_id` for a one-object request) | `duration_ms`, `object_count`, `clu_count`, `request_bytes`, `success`, `queue_wait_ms`, `overlap` | write request |
| `grengate_command` | `operation=command_endtoend`, `component=command`, `clu_id`, `object_id` | `duration_ms` (submit to result), `queue_wait_ms` (submit to sent), `http_ms` | command (superseded commands are not reported) |
| `grengate_refresh` | `operation=flush`, `component=gate_broker` (+ `clu_id`, `object_id` for a one-object request) | same as the write request | read request |
| `grengate_refresh` | `operation=refresh_cycle`, `component=grenton_set` | `duration_ms`, `object_count`, `changed_objects`, `skipped_objects`, `discarded_objects`, `missing_objects`, `failed_objects` | refresh |

`overlap` is 1 when a request of the other endpoint (read or write) was in flight during this one.
In `refresh_cycle`, `object_count` counts the devices read in that refresh: `applied`, `discarded_objects`, `missing_objects` and
`failed_objects` add up to it. `skipped_objects` counts the thermostats not due in that cycle; they are not part of `object_count`.
Points are buffered and sent every 10 s; if InfluxDB is unreachable they are dropped (and the drop is logged), grengate never waits for it.

#### autotest

`-do-autotest` (or `"PerformAutotest": true`) reads every configured device once at startup and logs each device
for which the GATE returned no item, an item of another kind, or an item without its payload. Failing devices stay configured.

### grenton GATE

The GATE needs two HTTP listener objects with a Lua script each: `hb_multi_gate` (read, `grenton/read-script.lua`) and
`homebridge` (write, `grenton/update-script.lua`); point `ReadPath` and `SetLightPath` at them.
The request and response contract, the rollout order and the manual check are in [grenton/README.md](grenton/README.md).

## remaining manual steps

The Lua scripts cannot be tested from Go. Before relying on this version, deploy the scripts and run the manual check of
stage 5 in `IMPLEMENTATION_PLAN.md` (also in `grenton/README.md`) on the real GATE, then take the measurements below and
adjust the defaults if they show a need.

## measurements

To be filled in from the real installation (InfluxDB metrics enabled). Nothing has been measured yet.

| What | Where it comes from | Result |
|---|---|---|
| Write request duration against item count | `grengate_command,operation=flush`: `duration_ms` against `object_count` | not measured yet |
| Queue wait of commands and requests | `grengate_command,operation=command_endtoend`: `queue_wait_ms` (and `duration_ms` against `http_ms`); `operation=flush` points: `queue_wait_ms` | not measured yet |
| Lane overlap (reads and writes in flight together), and whether writes are slower then | `overlap` on both `flush` points; compare write `duration_ms` with `overlap=1` and `0` | not measured yet |
| Shutter position error on partial moves | no metric: move a shutter to a partial position from HomeKit and compare the real position; the delays behind the error are the write `duration_ms` per `object_count` and the `STOP` command's `queue_wait_ms` | not measured yet |

From these, `SetterQueueSize`, `SetterFlushMs` and `QueryLimit` may need new defaults (a smaller `QueryLimit` if writes suffer while reads are in flight).


## changelog

### v0.3

Added thermostat object, did some code refactor.

### v0.2

No more js Homebridge! Using [https://github.com/brutella/hc](https://github.com/brutella/hc) package it is a standalone app acting as HomeKit accessory and connecting to Grenton system.

So far only simple Light object present, using only Grenton DOUT.

### v0.1

First working version, it is a queue between node.js Homebridge and Grenton Gate.
Gate module simply couldn't keep up with many http requests, so I made this go app.