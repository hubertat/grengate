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
		fmt.Printf("grengate %s\n", Version)
		fmt.Printf("Git Commit: %s\n", GitCommit)
		fmt.Printf("Build Date: %s\n", BuildDate)
		return
	}

	gren := GrentonSet{}
	err := gren.Config(*configPath)
	if err != nil {
		log.Fatalf("GrentonSet Config failed: %v", err)
	}

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	gren.InitClus()
	gren.startWorkers()
	defer gren.Stop()

	// Autotest, startup refresh and refresh cycle: HomeKit starts only after the startup
	// refresh has an outcome for every device.
	gren.startup(ctx, gren.PerformAutotest || *performAutotest)

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

	err = server.ListenAndServe(ctx)
	if err != nil {
		log.Fatal(err)
	} else {
		log.Println("grengate exiting, bye.")
	}
}
