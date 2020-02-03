package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/brutella/hc/accessory"
)

type GrentonClu struct {
	Id     string
	Name   string
	Lights []*GrentonLight

	grentonSet *GrentonSet
	block      sync.Mutex
}

func (gc *GrentonClu) GetIntId() uint32 {
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
		gc.grentonSet.Error(err)
	}
	return uint32(uVal)

}
func (gc *GrentonClu) GetMixedId() string {
	return gc.Id
}
func (gc *GrentonClu) InitLights() {
	for _, light := range gc.Lights {
		light.clu = gc
		light.AppendHk()
	}
}

func (gc *GrentonClu) GetAllHkAcc() (slc []*accessory.Accessory) {
	slc = []*accessory.Accessory{}

	for _, light := range gc.Lights {
		slc = append(slc, light.HkAcc.Accessory)
	}

	return
}