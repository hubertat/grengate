# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**grengate** is a HomeKit gateway for Grenton home automation systems, written in Go. It acts as a bridge between the Grenton home automation system and Apple HomeKit, enabling control of Grenton devices through HomeKit.

The application uses the [brutella/hap](https://github.com/brutella/hap) framework to implement the HomeKit Accessory Protocol (HAP).

## Build & Run Commands

### Build
```bash
go build -o grengate
```

### Run
```bash
# Default config path (./config.json)
./grengate

# Custom config path
./grengate -config /path/to/config.json

# With autotest on startup
./grengate -do-autotest
```

### Dependencies
```bash
# Install/update dependencies
go mod download

# Tidy dependencies
go mod tidy
```

### Cross-compilation (for Linux deployment)
```bash
GOOS=linux GOARCH=amd64 go build -o grengate_linux
```

## Configuration

Configuration is JSON-based. See `config.json.example` for structure:

- **HkPin/HkPath**: HomeKit pairing PIN and pairing storage directory
- **Host/ReadPath/SetLightPath**: Grenton GATE HTTP endpoints
- **FreshInSeconds**: Accepted for old config files, no effect
- **CycleInSeconds**: Periodic refresh interval (default: 10s)
- **QueryLimit**: Devices per read request (default: 30)
- **InfluxDB** (`Enabled`, `URL`, `Token`, `Org`, `Bucket`): Optional InfluxDB v2 metrics, off by default
- **InputServerPort**: Optional HTTP server port for receiving Grenton push updates (motion sensors)
- **BridgeName**: HomeKit bridge name (default: "grengate")
- **Clus**: Array of CLU objects with their devices (Lights, Therms, Shutters, MotionSensors)

## Architecture

### Core Concepts

1. **GrentonSet** (`grenton_set.go`) - Main coordinator struct
   - Loads configuration
   - Manages multiple CLUs (Grenton Central Logic Units)
   - Handles periodic refresh cycles
   - Coordinates HomeKit bridge setup
   - Owns the read lane and the write lane, the device registry, the HomeKit syncer and the metrics

2. **Clu** (`clu.go`) - Represents a Grenton CLU controller
   - Contains collections of devices: Lights, Thermostats, Shutters, MotionSensors
   - CLU IDs can be hex (e.g., "CLU_012abcde") or decimal format
   - Each device references its parent CLU

3. **Lanes** (`lane.go`, `gate_client.go`) - one per GATE endpoint (concept 3.1)
   - Mailbox keyed by Clu+Id (merge policy per lane: reads merge, writes replace with a "superseded" result) and one worker sending one request at a time
   - `Submit` never blocks; each submission gets a one-slot result channel; results carry a completion stamp shared by both lanes
   - Read lane: up to `QueryLimit` devices per array request. Write lane: one single-object command per request (legacy protocol)
   - Hooks set by `GrentonSet`: `onSent` (request about to be sent) and `onComplete` (per entry, before delivery); neither may block
   - **Critical**: Grenton GATE cannot handle many rapid HTTP requests, so each lane sends strictly one request at a time

3a. **Registry** (`registry.go`) - devices by Clu+Id (case-insensitive) behind the small `device` interface (`applyRead`, `Sync`, `readRequest`)

4. **Device Types** - Each implements HomeKit accessories
   - **Light** (`light.go`): Binary on/off DOUT devices. `HkType` selects the HomeKit accessory: `Lightbulb` (default) or `Outlet`; the Grenton side (Kind `Light`) is identical for both
   - **Thermo** (`thermo.go`): Thermostats with target/current temperature
   - **Shutter** (`shutter.go`): Window coverings with position control and simulated movement
   - **MotionSensor** (`motion_sensor.go`): Binary motion detection

5. **CluObject** (`clu_object.go`) - Base struct for all devices
   - Common fields: Id, Name, Kind
   - Provides `GetMixedId()` (e.g., "DOU0001") and `GetLongId()` (unique 64-bit)
   - Contains `Req` (ReqObject, the read request item)
   - `mu` guards device state (never held during HTTP or hap calls), `syncMu` serialises HomeKit pushes
   - `sendCommand()` submits to the write lane and waits for the result (callbacks become non-blocking in stage 3)

6. **InputServer** (`input_server.go`) - Optional HTTP server
   - Listens for push updates from Grenton (primarily for motion sensors)
   - Endpoint: `/update` (POST, JSON payload with Clu/Id/State)
   - Enables real-time updates instead of polling

### Data Flow

**Reading State:**
1. `GrentonSet.Refresh()` (`refresh.go`) submits every device to the read lane at once and waits for an outcome per device
2. The read lane POSTs arrays of up to `QueryLimit` items to `ReadPath`
3. The completion hook (`readCompleted`) finds the device in the registry, `applyRead` validates and stores the item, and the HomeKit syncer (`hk_sync.go`) pushes it to HomeKit
4. Refresh counts applied/changed/discarded/missing-or-invalid/failed and reports a metrics event
5. One refresh runs at startup before the HomeKit server (`startup()`), then `refreshScheduler` (`scheduler.go`) runs one per cycle boundary, skipping boundaries that pass during a refresh

**Writing State:**
1. HomeKit accessory callback triggered (e.g., `Light.Set()`)
2. Device updates local state and builds a command snapshot under its lock
3. `sendCommand()` submits it to the write lane and waits for the result
4. The write lane POSTs it as a single object to `SetLightPath`; the response is an acknowledgement only (`decodeWriteResponse`, both response shapes) and is never loaded as state

**Push Updates (Motion Sensors):**
1. Grenton GATE Lua script POSTs to InputServer `/update` endpoint
2. InputServer finds the motion sensor in the registry
3. Device `Set()` method updates state and syncs to HomeKit

### HomeKit Integration

- **Bridge Pattern**: One bridge accessory containing all device accessories
- **Accessory IDs**: Generated via `GetLongId()` (combines CLU ID and device ID)
- **Storage**: HAP framework stores pairing data in `hk/` directory (HkPath config)
- **Callbacks**: HomeKit changes trigger device methods (OnValueRemoteUpdate)
- **Sync Pattern**: Devices call `.SetValue()` on HomeKit characteristics to push updates

### Key Design Patterns

1. **Lanes**: one request in flight per GATE endpoint, latest command wins per device
2. **Periodic Refresh**: fixed cycle boundaries, no `time.Ticker`, one refresh at a time
3. **Mixed IDs**: Devices use "KIND####" format (e.g., "DOU0001") as unique identifiers within CLUs
4. **Metrics** (`metrics.go`, `influx.go`): `Metrics` interface (request, command, refresh events), no-op by default, InfluxDB v2 line protocol when configured
5. **Shutter Simulation**: Shutters simulate movement progress with tickers since Grenton doesn't provide position feedback

## Development Notes

- **Tests**: `go test -race ./...`. `fakegate_test.go` is a fake GATE (API described at the top of the file); `helpers_test.go` has `newTestSet`/`newTestClu` (a `GrentonSet` wired to it without a config file), `captureLog`, `eventually`; `light_test.go` has `findC`
- **Logging**: Use `GrentonSet.Logf()` for normal logs, `Debugf()` for verbose (controlled by Verbose config)
- **Error Handling**: Errors logged via `GrentonSet.Error()`, often with `pkg/errors.Wrap/Wrapf`
- **Concurrency**: device state behind `CluObject.mu`; lane mailboxes behind their own mutex, never held during HTTP
- **Timeout**: every GATE request is bounded by `GrentonSet.requestTimeout` (default 10 s, tests shorten it)
- **Version**: `Version`, `GitCommit` and `BuildDate` variables in `main.go`, set via ldflags at build time (`--version` prints them)

## Deployment

Typically deployed as a systemd service on Linux (see README.md for service configuration). The service runs continuously, maintaining HomeKit pairing and polling Grenton GATE for device state changes.

### Lua Script Requirement

A Lua script must run on the Grenton GATE module to handle HTTP requests from grengate. This script:
- Responds to read requests with device states
- Executes set commands on devices
- (Optional) Pushes motion sensor updates to InputServer
