package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/claes/pulseaudio-mqtt/lib"
)

var debug *bool

func printHelp() {
	fmt.Println("Usage: pulseaudio-mqtt [OPTIONS]")
	fmt.Println("Options:")
	flag.PrintDefaults()
}

func main() {
	pulseServer := flag.String("pulseserver", "", "Pulse server address")
	mqttBroker := flag.String("broker", "tcp://localhost:1883", "MQTT broker URL")
	help := flag.Bool("help", false, "Print help")
	debug = flag.Bool("debug", false, "Debug logging")
	flag.Parse()

	if *help {
		printHelp()
		os.Exit(0)
	}

	bridge := lib.NewPulseaudioMQTTBridge(
		lib.CreatePulseClient(*pulseServer), lib.CreateMQTTClient(*mqttBroker))

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	fmt.Printf("Started\n")
	go bridge.MainLoop()
	<-c
	bridge.PulseClient.Close()
	fmt.Printf("Shut down\n")

	os.Exit(0)
}
