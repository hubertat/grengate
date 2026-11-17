package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeItem(t *testing.T, s string) ReqObject {
	t.Helper()
	var obj ReqObject
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return obj
}

// Validity per kind (concept 3.5): the payload with every field grengate uses. A field the
// script left out is not the same as a zero value.
func TestPayload_ValidityPerKind(t *testing.T) {
	thermoAll := `"TempCurrent":20,"TempTarget":21,"TempSetpoint":21,"State":0,"Mode":0`
	cases := []struct {
		name, item string
		valid      bool
		errPart    string
	}{
		{"light", `{"Kind":"Light","Light":{"State":false}}`, true, ""},
		{"light no payload", `{"Kind":"Light"}`, false, "missing Light payload"},
		{"light no State", `{"Kind":"Light","Light":{}}`, false, "State"},
		{"light wrong kind", `{"Kind":"Thermo","Light":{"State":true}}`, false, "wrong object kind"},
		{"motion", `{"Kind":"MotionSensor","MotionSensor":{"State":false}}`, true, ""},
		{"motion no payload", `{"Kind":"MotionSensor"}`, false, "missing MotionSensor payload"},
		{"motion no State", `{"Kind":"MotionSensor","MotionSensor":{}}`, false, "State"},
		{"thermo", `{"Kind":"Thermo","Thermo":{` + thermoAll + `}}`, true, ""},
		{"thermo zero values", `{"Kind":"Thermo","Thermo":{"TempCurrent":0,"TempTarget":0,"TempSetpoint":0,"State":0,"Mode":0}}`, true, ""},
		{"thermo no payload", `{"Kind":"Thermo"}`, false, "missing Thermo payload"},
		{"thermo no TempCurrent", `{"Kind":"Thermo","Thermo":{"TempTarget":21,"TempSetpoint":21,"State":0,"Mode":0}}`, false, "TempCurrent"},
		{"thermo no Mode", `{"Kind":"Thermo","Thermo":{"TempCurrent":20,"TempTarget":21,"TempSetpoint":21,"State":0}}`, false, "Mode"},
		{"thermo unused fields only", `{"Kind":"Thermo","Thermo":{"TempMin":5,"TempMax":30,"TempHoliday":10}}`, false, "TempSetpoint"},
		{"shutter", `{"Kind":"Shutter","Shutter":{"State":0,"MaxTime":20000}}`, true, ""},
		{"shutter MaxTime 100", `{"Kind":"Shutter","Shutter":{"State":1,"MaxTime":100}}`, true, ""},
		{"shutter MaxTime 0", `{"Kind":"Shutter","Shutter":{"State":0,"MaxTime":0}}`, false, "below"},
		{"shutter MaxTime 99", `{"Kind":"Shutter","Shutter":{"State":0,"MaxTime":99}}`, false, "below"},
		{"shutter no MaxTime", `{"Kind":"Shutter","Shutter":{"State":0}}`, false, "MaxTime"},
		{"shutter no State", `{"Kind":"Shutter","Shutter":{"MaxTime":20000}}`, false, "State"},
		{"shutter no payload", `{"Kind":"Shutter"}`, false, "missing Shutter payload"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := decodeItem(t, c.item)
			var err error
			switch {
			case strings.HasPrefix(c.name, "light"):
				_, err = validLight(obj)
			case strings.HasPrefix(c.name, "motion"):
				_, err = validMotion(obj)
			case strings.HasPrefix(c.name, "thermo"):
				_, err = validThermo(obj)
			case strings.HasPrefix(c.name, "shutter"):
				_, err = validShutter(obj)
			}
			if c.valid && err != nil {
				t.Errorf("valid item rejected: %v", err)
			}
			if !c.valid && (err == nil || !strings.Contains(err.Error(), c.errPart)) {
				t.Errorf("err = %v, want an error mentioning %q", err, c.errPart)
			}
		})
	}
}

// Commands carry only the fields set, and a false or zero value is still sent.
func TestPayload_CommandEncoding(t *testing.T) {
	light := ReqObject{Clu: tCLU, Id: "DOU0001", Kind: "Light", Cmd: "SET", Light: &LightPayload{State: ptr(false)}, seq: 7}
	b, _ := json.Marshal(light)
	if got, want := string(b), `{"Clu":"CLU_012abcde","Id":"DOU0001","Kind":"Light","Cmd":"SET","Light":{"State":false}}`; got != want {
		t.Errorf("light command = %s, want %s", got, want)
	}
	thermo := ReqObject{Clu: tCLU, Id: "THE0002", Kind: "Thermo", Cmd: "SET", Source: "sensor",
		Thermo: &ThermoPayload{TempSetpoint: ptr(21.5), State: ptr(0), Mode: ptr(0)}}
	b, _ = json.Marshal(thermo)
	if got, want := string(b), `{"Clu":"CLU_012abcde","Id":"THE0002","Kind":"Thermo","Cmd":"SET","Source":"sensor","Thermo":{"TempSetpoint":21.5,"State":0,"Mode":0}}`; got != want {
		t.Errorf("thermostat command = %s, want %s", got, want)
	}
}
