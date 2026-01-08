# PR #1 Review: Upgrade to HAP

## Overview
This PR upgrades from the older `brutella/hc` v1.2.4 library to the newer `brutella/hap` v0.0.20 library, along with several feature additions.

## Critical Issues

### 1. Build Failure - BLOCKING ⛔
**Status**: Build fails with Go 1.17 (specified in go.mod) but succeeds with updated dependencies

**Error**:
```
link: golang.org/x/net/internal/socket: invalid reference to syscall.recvmsg
```

**Root Cause**: The `brutella/hap` v0.0.20 requires newer versions of golang.org/x dependencies than what's in go.sum. The dependencies in the PR are from 2021-2022, but Go 1.25.4 (current on system) requires newer versions.

**Solution Required**:
- Run `go get -u golang.org/x/net && go mod tidy` to update dependencies
- Consider updating `go 1.17` to `go 1.19` or higher in go.mod
- Commit the updated go.mod and go.sum

---

### 2. Breaking Config Change - CRITICAL ⚠️
**Status**: **Existing configs will break without migration**

**Removed Field**: `HkSetupId`
- Old config had: `"HkSetupId": "ABCD"`
- New config doesn't use this field
- The new HAP library doesn't require/support SetupId

**Added Fields** (with defaults, backward compatible):
- `"HkPath"` - defaults to "hk" directory (storage path for HAP data)
- `"BridgeName"` - defaults to "grengate" if not specified
- `"InputServerPort"` - optional, no input server if not specified

**Impact**:
✅ **Config is mostly backward compatible!**
- Users with existing configs can keep them as-is
- `HkSetupId` will be ignored (not an error)
- Missing `HkPath` defaults to "hk" (matches old hardcoded behavior)
- Missing `BridgeName` defaults to "grengate" (matches old behavior)
- Missing `InputServerPort` means feature is disabled

**Recommendation**:
- ✅ Document in README that `HkSetupId` is no longer used
- ✅ Add migration note that existing pairings should continue to work
- ⚠️ Users may need to re-pair devices if HAP storage format changed between hc → hap

---

## New Features

### 3. Input Server (HTTP Push Support)
**Files**: `input_server.go` (new)

**Purpose**: Allows Grenton GATE to push motion sensor updates in real-time instead of polling

**Configuration**:
```json
"InputServerPort": 8080
```

**API**: `POST /update` with JSON payload:
```json
{
  "Clu": "CLU_xyz",
  "Id": "DIN0001",
  "State": true
}
```

**Security Concerns**: ⚠️
- No authentication on the input server
- Accepts any POST request from any source
- Consider adding:
  - IP whitelist (only accept from Grenton GATE IP)
  - Shared secret/token in headers
  - Rate limiting

**Recommendation**: Document security considerations

---

### 4. Motion Sensor Support
**Files**: `motion_sensor.go` (new)

**Features**:
- Exposed as HomeKit motion sensors
- Auto-clear after 15 seconds (configurable via `clearMotionEventDuration`)
- Can receive updates via Input Server or polling

**Config Example**:
```json
"MotionSensors": [
  {
    "Id": 2881,
    "Kind": "DIN",
    "Name": "hallway sensor"
  }
]
```

**Implementation Notes**:
- Uses custom accessory construction (not a built-in accessory type)
- Includes StatusFault characteristic for reporting sensor issues

---

### 5. Shutter Improvements
**Removed**: `shutter_simple.go` (simple switch-based shutter)
**Enhanced**: `shutter.go` now simulates position tracking

**Breaking Change**:
- Old config with `"SimpleShutters"` array **will break**
- Must rename to `"Shutters"` in config

**New Features**:
- Position tracking (0-100%)
- Simulated movement based on `MaxTime` config
- Proper HomeKit WindowCovering accessory (not a switch)
- Commands: MOVEUP, MOVEDOWN, STOP

**Recommendation**:
- ⚠️ Add migration note about `SimpleShutters` → `Shutters` rename
- ⚠️ Users with existing SimpleShutters configs will get errors

---

## API/Library Changes

### 6. HAP Library API Changes
**Package**: `github.com/brutella/hc` → `github.com/brutella/hap`

**Major Changes**:

1. **Accessory Creation**:
   ```go
   // OLD (hc)
   info := accessory.Info{
       ID: 123,  // ID in constructor
   }
   light := accessory.NewLightbulb(info)

   // NEW (hap)
   info := accessory.Info{
       // No ID field
   }
   light := accessory.NewLightbulb(info)
   light.Id = 123  // Set after creation
   ```

2. **Accessory Reference**:
   ```go
   // OLD: light.hk.Accessory.ID
   // NEW: light.hk.A.Id
   ```

3. **Server Setup**:
   ```go
   // OLD (hc)
   config := hc.Config{Pin: "1234", SetupId: "ABCD", StoragePath: "hk"}
   transport, _ := hc.NewIPTransport(config, bridge.Accessory, accessories...)
   transport.Start()

   // NEW (hap)
   fs := hap.NewFsStore("hk")
   server, _ := hap.NewServer(fs, bridge.A, accessories...)
   server.Pin = "1234"
   server.ListenAndServe(ctx)
   ```

4. **Shutdown Handling**:
   ```go
   // OLD: hc.OnTermination(func() { ... })
   // NEW: Context-based with signal handling
   ```

5. **Thermostat Constructor**:
   ```go
   // OLD: accessory.NewThermostat(info, defaultTemp, minTemp, maxTemp, step)
   // NEW: accessory.NewThermostat(info)  // No params
   ```

6. **Removed Callbacks**:
   - `OnValueRemoteGet()` callbacks commented out (may not be supported in hap)
   - Only `OnValueRemoteUpdate()` used now

---

## Code Quality Issues

### 7. Debug Config File Committed
**File**: `config.json~`

⚠️ **This appears to be a backup/temp file with real config data**
- Contains actual IP address: `10.100.81.73`
- Contains actual HomeKit PIN: `40004000`
- Should NOT be in repository

**Action Required**:
```bash
git rm config.json~
echo "config.json~" >> .gitignore
```

---

### 8. Command-Line Flags
**New Feature**: Proper flag parsing

✅ Good additions:
```bash
./grengate -config /path/to/config.json
./grengate -do-autotest
```

---

## Testing Recommendations

### Manual Testing Checklist:

1. **Fresh Install** (no existing pairing)
   - [ ] Start grengate with new config
   - [ ] Pair with HomeKit using QR code/PIN
   - [ ] Verify all devices appear
   - [ ] Test controlling lights, thermostats, shutters
   - [ ] Test motion sensor updates

2. **Upgrade Path** (existing pairing from old version)
   - [ ] Stop old grengate (hc version)
   - [ ] Start new grengate (hap version) with same config
   - [ ] Check if pairing persists (hk/ directory compatibility)
   - [ ] If pairing breaks, document re-pairing process

3. **Input Server**
   - [ ] Configure InputServerPort
   - [ ] Send test POST to `/update` endpoint
   - [ ] Verify motion sensor updates in HomeKit
   - [ ] Test with invalid/malformed JSON

4. **Shutters**
   - [ ] Test position setting (0%, 50%, 100%)
   - [ ] Verify movement simulation works
   - [ ] Test STOP command mid-movement

5. **Config Compatibility**
   - [ ] Test old config (with HkSetupId) - should work
   - [ ] Test config without HkPath - should default to "hk"
   - [ ] Test config with SimpleShutters - should fail gracefully

---

## Security Review

### Concerns:
1. ⚠️ Input Server has no authentication
2. ⚠️ Hardcoded storage path "hk" if not in config (minor)
3. ⚠️ config.json~ contains sensitive data in repo

### Recommendations:
1. Add authentication to Input Server (token, IP whitelist)
2. Remove config.json~ from repo
3. Update .gitignore for backup files

---

## Documentation Updates Needed

Update README.md with:
1. Migration guide from hc → hap version
   - May require re-pairing with HomeKit
   - `HkSetupId` no longer used
   - `SimpleShutters` renamed to `Shutters`

2. New config options:
   - `HkPath` (optional, default: "hk")
   - `BridgeName` (optional, default: "grengate")
   - `InputServerPort` (optional, enables push updates)
   - `MotionSensors` array in CLU config

3. Input Server API documentation
   - Endpoint: `POST /update`
   - Payload format
   - Security considerations

4. Lua script updates (if needed for Input Server)

---

## Summary

### Blocking Issues:
1. ⛔ **Build fails** - needs dependency update (go mod tidy)
2. ⛔ **config.json~** must be removed from repo

### Breaking Changes:
1. ⚠️ `SimpleShutters` → `Shutters` (config field rename)
2. ℹ️ `HkSetupId` ignored (not breaking, just obsolete)
3. ⚠️ May require HomeKit re-pairing (HAP storage format changes)

### Risk Assessment:
- **Config Compatibility**: 🟡 Medium - mostly compatible, but SimpleShutters breaks
- **Code Quality**: 🟢 Good - clean refactoring for HAP library
- **Testing**: 🟡 Unknown - no tests, needs manual validation
- **Security**: 🟡 Medium - Input Server needs hardening

### Recommendation:
**Approve with required fixes:**
1. Fix build (update dependencies, commit go.mod/go.sum)
2. Remove config.json~
3. Document breaking changes in PR description or README
4. Consider adding authentication to Input Server

**After merge:**
- Test upgrade path with existing installation
- Update documentation
- Add security hardening for Input Server
