package main

import (
	// "fmt"
	"log"

	"github.com/brutella/hc"
	"github.com/brutella/hc/accessory"
)

func main() {
	log.Print("Starting grengate")

	configPath := "./config.json"
	gren := GrentonSet{}
	err := gren.Config(configPath)
	if err != nil {
		log.Fatalf("GrentonSet Config failed: %v", err)
	}

	gren.InitClus()

	if gren.PerformAutotest {
		log.Print("Testing all Grenton elements")
		gren.TestAllGrentonGate()
	}

	log.Print("Starting update cycles")
	gren.StartCycling()

	log.Print("HomeKit init")
	
	info := accessory.Info{
		Name: "grengate",
		Manufacturer: "github.com/hubertat",
		FirmwareRevision: "v0.3",
		ID: 1,
	}
	bridge := accessory.NewBridge(info)
	config := hc.Config{
		Pin:     gren.HkPin,
		SetupId: gren.HkSetupId,
		StoragePath: "hk",
	}
	t, err := hc.NewIPTransport(config, bridge.Accessory, gren.GetAllHkAcc()...)
	if err != nil {
		log.Panic(err)
	}

	hc.OnTermination(func() {
		<-t.Stop()
	})

	t.Start()

}
