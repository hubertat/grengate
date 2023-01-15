package main

import (
	// "fmt"
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/pkg/errors"
)

const grengateVer = "v0.5"

func main() {
	log.Print("Starting grengate")

	ctx := context.Background()

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
		Name:         "grengate",
		Manufacturer: "github.com/hubertat",
		Firmware:     grengateVer,
	}
	bridge := accessory.NewBridge(info)
	bridge.Id = 1

	fs := hap.NewFsStore(gren.HkPath)

	server, err := hap.NewServer(fs, bridge.A, gren.GetAllHkAcc()...)
	if err != nil {
		err = errors.Wrap(err, "failed to create new hap server")
		log.Fatal(err)
	}

	server.Pin = gren.HkPin

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(ctx)
	go func() {
		<-c
		// Stop delivering signals.
		signal.Stop(c)
		// Cancel the context to stop the server.
		cancel()
	}()

	err = server.ListenAndServe(ctx)
	if err != nil {
		log.Fatal(err)
	} else {
		log.Println("grengate exiting, bye.")
	}
}
