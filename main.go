package main

import (
	"flag"
	"fmt"
	"log/slog"
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
	topicPrefix := flag.String("topicPrefix", "", "MQTT topic prefix to use")
	mqttBroker := flag.String("broker", "tcp://localhost:1883", "MQTT broker URL")
	help := flag.Bool("help", false, "Print help")
	debug = flag.Bool("debug", false, "Debug logging")
	flag.Parse()

	if *help {
		printHelp()
		os.Exit(0)
	}

	pulseClient, err := lib.CreatePulseClient(*pulseServer)
	if err != nil {
		slog.Error("Error creating pulse client", "error", err, "pulseServer", *pulseServer)
		os.Exit(1)
	}

	mqttClient, err := lib.CreateMQTTClient(*mqttBroker)
	if err != nil {
		slog.Error("Error creating mqtt client", "error", err, "broker", *mqttBroker)
		os.Exit(1)
	}
	bridge := lib.NewPulseaudioMQTTBridge(pulseClient, mqttClient, *topicPrefix)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	fmt.Printf("Started\n")
	go bridge.MainLoop()
	<-c
	bridge.PulseClient.Close()
	fmt.Printf("Shut down\n")

	os.Exit(0)
}
