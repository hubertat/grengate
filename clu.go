package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/brutella/hc/accessory"
)

type Clu struct {
	Id     string
	Name   string

	Lights []*Light
	Therms []*Thermo
	Shutters []*Shutter
	SimpleShutters []*ShutterSimple

	set   *GrentonSet
	block sync.Mutex
}

func (gc *Clu) GetIntId() uint32 {
	cluIdS := gc.Id[3:]
	var base int
	if strings.HasPrefix(cluIdS, "_") {
		base = 16
	} else {
		base = 10
	}
	uVal, err := strconv.ParseUint(cluIdS[1:], base, 32)
	if err != nil {
		err = fmt.Errorf("Converting clu id [%s] (to uint) failed: %v", gc.Id, err)
		gc.set.Error(err)
	}
	return uint32(uVal)

}
func (gc *Clu) GetMixedId() string {
	return gc.Id
}
func (gc *Clu) InitAll() {
	for _, light := range gc.Lights {
		light.clu = gc
		light.InitAll()
	}

	for _, thermo := range gc.Therms {
		thermo.clu = gc
		thermo.InitAll()
	}
	for _, sht := range gc.SimpleShutters {
		sht.clu = gc
		sht.InitAll()
	}
}

func (gc *Clu) GetAllHkAcc() (slc []*accessory.Accessory) {
	slc = []*accessory.Accessory{}

	for _, light := range gc.Lights {
		slc = append(slc, light.hk.Accessory)
	}

	for _, thermo := range gc.Therms {
		slc = append(slc, thermo.hk.Accessory)
	}
	for _, sht := range gc.SimpleShutters {
		slc = append(slc, sht.hk.Accessory)
	}

	return
}
