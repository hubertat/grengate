package main

import (
	// "fmt"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/pkg/errors"
)

// Version information (set via ldflags during build)
var (
	Version   = "dev"     // Git tag or version
	GitCommit = "unknown" // Git commit hash
	BuildDate = "unknown" // Build timestamp
)

func main() {
	log.Print("Starting grengate")

	ctx := context.Background()

	configPath := flag.String("config", "./config.json", "config file path")
	performAutotest := flag.Bool("do-autotest", false, "perform an autotest on startup")
	showVersion := flag.Bool("version", false, "Show version information and exit")
	flag.Parse()

	// Show version and exit if requested
	if *showVersion {
		fmt.Printf("blesrv %s\n", Version)
		fmt.Printf("Git Commit: %s\n", GitCommit)
		fmt.Printf("Build Date: %s\n", BuildDate)
		return
	}

	gren := GrentonSet{}
	err := gren.Config(*configPath)
	if err != nil {
		log.Fatalf("GrentonSet Config failed: %v", err)
	}

	gren.InitClus()

	if gren.PerformAutotest || *performAutotest {
		log.Print("Testing all Grenton elements")
		gren.TestAllGrentonGate()
	}

	log.Println("Starting update cycles")
	gren.StartCycling()

	if gren.InputServerPort > 0 {
		log.Printf("Starting input server (listening on port %d)\n", gren.InputServerPort)
		grentonIn := NewInputServer(&gren, gren.InputServerPort)
		go func() {
			log.Fatal(grentonIn.Run())
		}()
	} else {
		log.Println("Input server disabled")
	}

	log.Print("HomeKit init")

	bridgeName := gren.BridgeName
	if len(bridgeName) == 0 {
		bridgeName = "grengate"
	}

	info := accessory.Info{
		Name:         bridgeName,
		Manufacturer: "github.com/hubertat",
		Model:        "grengate",
		Firmware:     Version,
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
