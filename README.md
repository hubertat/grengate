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

`FreshInSeconds` is no longer used. It is still accepted, so older config files load unchanged.

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
Points are buffered and sent every 10 s; if InfluxDB is unreachable they are dropped (and the drop is logged), grengate never waits for it.

#### autotest

`-do-autotest` (or `"PerformAutotest": true`) reads every configured device once at startup and logs each device
for which the GATE returned no item, an item of another kind, or an item without its payload. Failing devices stay configured.

### grenton GATE

#### http listener

#### lua script


## changelog

### v0.3

Added thermostat object, did some code refactor.

### v0.2

No more js Homebridge! Using [https://github.com/brutella/hc](https://github.com/brutella/hc) package it is a standalone app acting as HomeKit accessory and connecting to Grenton system.

So far only simple Light object present, using only Grenton DOUT.

### v0.1

First working version, it is a queue between node.js Homebridge and Grenton Gate.
Gate module simply couldn't keep up with many http requests, so I made this go app.