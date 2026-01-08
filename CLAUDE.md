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

- **HkPin/HkSetupId**: HomeKit pairing configuration
- **Host/ReadPath/SetLightPath**: Grenton GATE HTTP endpoints
- **FreshInSeconds**: Data freshness threshold (default: 3s)
- **CycleInSeconds**: Periodic refresh interval (default: 10s)
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
   - Contains two GateBrokers: one for reading state, one for setting state

2. **Clu** (`clu.go`) - Represents a Grenton CLU controller
   - Contains collections of devices: Lights, Thermostats, Shutters, MotionSensors
   - CLU IDs can be hex (e.g., "CLU_012abcde") or decimal format
   - Each device references its parent CLU

3. **GateBroker** (`gate_broker.go`) - HTTP request queue manager
   - Batches HTTP requests to avoid overwhelming the Grenton GATE module
   - Configurable queue length and flush period
   - Two instances: `broker` (reads) and `setter` (writes)
   - **Critical**: Grenton GATE cannot handle many rapid HTTP requests, so batching is essential

4. **Device Types** - Each implements HomeKit accessories
   - **Light** (`light.go`): Binary on/off lights (DOUT devices)
   - **Thermo** (`thermo.go`): Thermostats with target/current temperature
   - **Shutter** (`shutter.go`): Window coverings with position control and simulated movement
   - **MotionSensor** (`motion_sensor.go`): Binary motion detection

5. **CluObject** (`clu_object.go`) - Base struct for all devices
   - Common fields: Id, Name, Kind
   - Provides `GetMixedId()` (e.g., "DOU0001") and `GetLongId()` (unique 64-bit)
   - Contains `Req` (ReqObject) for HTTP communication
   - Methods: `Update()`, `SendReq()`, `TestGrentonGate()`

6. **InputServer** (`input_server.go`) - Optional HTTP server
   - Listens for push updates from Grenton (primarily for motion sensors)
   - Endpoint: `/update` (POST, JSON payload with Clu/Id/State)
   - Enables real-time updates instead of polling

### Data Flow

**Reading State:**
1. `GrentonSet.Refresh()` collects all ReqObjects from all devices
2. ReqObjects queued to `broker` GateBroker
3. Broker batches requests and sends POST to Grenton GATE `ReadPath`
4. Response parsed back into ReqObjects with updated state
5. `GrentonSet.update()` routes responses to correct device via `Find*()` methods
6. Device `LoadReqObject()` updates internal state and calls `Sync()` to update HomeKit accessory

**Writing State:**
1. HomeKit accessory callback triggered (e.g., `Light.Set()`)
2. Device updates local state
3. ReqObject with device data sent via `SendReq()` to `setter` GateBroker
4. Broker sends POST to Grenton GATE `SetLightPath`

**Push Updates (Motion Sensors):**
1. Grenton GATE Lua script POSTs to InputServer `/update` endpoint
2. InputServer finds device via `FindMotionSensor()`
3. Device `Set()` method updates state and syncs to HomeKit

### HomeKit Integration

- **Bridge Pattern**: One bridge accessory containing all device accessories
- **Accessory IDs**: Generated via `GetLongId()` (combines CLU ID and device ID)
- **Storage**: HAP framework stores pairing data in `hk/` directory (HkPath config)
- **Callbacks**: HomeKit changes trigger device methods (OnValueRemoteUpdate)
- **Sync Pattern**: Devices call `.SetValue()` on HomeKit characteristics to push updates

### Key Design Patterns

1. **Request Batching**: GateBroker queues requests to avoid overwhelming Grenton GATE
2. **Freshness Checking**: `CheckFreshness()` prevents redundant updates within threshold
3. **Periodic Refresh**: Background goroutine calls `Refresh()` at configured intervals
4. **Mixed IDs**: Devices use "KIND####" format (e.g., "DOU0001") as unique identifiers within CLUs
5. **Async Updates**: Many operations run in goroutines with error channels
6. **Shutter Simulation**: Shutters simulate movement progress with tickers since Grenton doesn't provide position feedback

## Development Notes

- **No Tests**: Currently no test files in the codebase
- **Logging**: Use `GrentonSet.Logf()` for normal logs, `Debugf()` for verbose (controlled by Verbose config)
- **Error Handling**: Errors logged via `GrentonSet.Error()`, often with `pkg/errors.Wrap/Wrapf`
- **Concurrency**: Mutexes used in GateBroker and Clu to prevent race conditions
- **Timeout**: HTTP client uses 10-second timeout for requests
- **Version**: Current version string in `main.go` constant `grengateVer`

## Deployment

Typically deployed as a systemd service on Linux (see README.md for service configuration). The service runs continuously, maintaining HomeKit pairing and polling Grenton GATE for device state changes.

### Lua Script Requirement

A Lua script must run on the Grenton GATE module to handle HTTP requests from grengate. This script:
- Responds to read requests with device states
- Executes set commands on devices
- (Optional) Pushes motion sensor updates to InputServer
