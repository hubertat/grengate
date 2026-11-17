package main

// Wire payloads: what a read item or a command carries for each device kind, separate from
// the device structs. Every field is a pointer so that a field the Lua script left out can
// be told from a zero value (concept 3.5); omitempty leaves unset fields out of commands.

import "fmt"

// LightPayload is the payload of a light or outlet (Kind "Light").
type LightPayload struct {
	State *bool `json:",omitempty"`
}

// ThermoPayload is the payload of a thermostat. A read result carries every field; a
// command carries TempSetpoint, State and Mode (the fields update-script.lua sets).
// Fields of the read script that grengate does not use (TempMin, TempMax, TempHoliday) are
// not decoded.
type ThermoPayload struct {
	TempCurrent  *float64 `json:",omitempty"`
	TempTarget   *float64 `json:",omitempty"`
	TempSetpoint *float64 `json:",omitempty"`
	State        *int     `json:",omitempty"`
	Mode         *int     `json:",omitempty"`
}

// ShutterPayload is the payload of a shutter read (State: 0 stopped, 1 up, 2 down; MaxTime
// in ms). Shutter commands carry no payload, only Cmd.
type ShutterPayload struct {
	State   *int `json:",omitempty"`
	MaxTime *int `json:",omitempty"`
}

// MotionPayload is the payload of a motion sensor read.
type MotionPayload struct {
	State *bool `json:",omitempty"`
}

// ptr returns a pointer to a copy of v.
func ptr[T any](v T) *T { return &v }

// The valid* functions check a read item against the validity table of concept 3.5: the
// payload of the device's kind with every field grengate uses. An invalid item is never
// applied.

func checkKind(obj ReqObject, kind string) error {
	if obj.Kind != kind {
		return fmt.Errorf("%s: wrong object kind (%s)", kind, obj.Kind)
	}
	return nil
}

func missingFields(kind string, fields ...string) error {
	return fmt.Errorf("%s: missing field %v in payload", kind, fields)
}

func validLight(obj ReqObject) (*LightPayload, error) {
	if err := checkKind(obj, "Light"); err != nil {
		return nil, err
	}
	p := obj.Light
	switch {
	case p == nil:
		return nil, fmt.Errorf("Light: missing Light payload")
	case p.State == nil:
		return nil, missingFields("Light", "State")
	}
	return p, nil
}

func validMotion(obj ReqObject) (*MotionPayload, error) {
	if err := checkKind(obj, "MotionSensor"); err != nil {
		return nil, err
	}
	p := obj.MotionSensor
	switch {
	case p == nil:
		return nil, fmt.Errorf("MotionSensor: missing MotionSensor payload")
	case p.State == nil:
		return nil, missingFields("MotionSensor", "State")
	}
	return p, nil
}

func validThermo(obj ReqObject) (*ThermoPayload, error) {
	if err := checkKind(obj, "Thermo"); err != nil {
		return nil, err
	}
	p := obj.Thermo
	if p == nil {
		return nil, fmt.Errorf("Thermo: missing Thermo payload")
	}
	var missing []string
	for _, f := range []struct {
		name    string
		present bool
	}{
		{"TempCurrent", p.TempCurrent != nil},
		{"TempTarget", p.TempTarget != nil},
		{"TempSetpoint", p.TempSetpoint != nil},
		{"State", p.State != nil},
		{"Mode", p.Mode != nil},
	} {
		if !f.present {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, missingFields("Thermo", missing...)
	}
	return p, nil
}

func validShutter(obj ReqObject) (*ShutterPayload, error) {
	if err := checkKind(obj, "Shutter"); err != nil {
		return nil, err
	}
	p := obj.Shutter
	switch {
	case p == nil:
		return nil, fmt.Errorf("Shutter: missing Shutter payload")
	case p.State == nil && p.MaxTime == nil:
		return nil, missingFields("Shutter", "State", "MaxTime")
	case p.State == nil:
		return nil, missingFields("Shutter", "State")
	case p.MaxTime == nil:
		return nil, missingFields("Shutter", "MaxTime")
	case *p.MaxTime < minShutterMaxTime:
		return nil, fmt.Errorf("Shutter: MaxTime %d ms is below %d ms", *p.MaxTime, minShutterMaxTime)
	}
	return p, nil
}
