package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/the-jonsey/pulseaudio"
)

var debug *bool

type PAState struct {
	DefaultSink          string
	ActiveProfilePerCard map[uint32]string
}

type PulseaudioMQTTBridge struct {
	MQTTClient      mqtt.Client
	PAClient        pulseaudio.Client
	PulseAudioState PAState
}

func NewPulseaudioMQTTBridge(mqttBroker string) *PulseaudioMQTTBridge {

	opts := mqtt.NewClientOptions().AddBroker(mqttBroker)
	mqttClient := mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		panic(token.Error())
	} else if *debug {
		fmt.Printf("Connected to MQTT broker: %s\n", mqttBroker)
	}

	paClient, err := pulseaudio.NewClient()
	if err != nil {
		panic(err)
	}

	bridge := &PulseaudioMQTTBridge{
		MQTTClient:      mqttClient,
		PAClient:        *paClient,
		PulseAudioState: PAState{"", make(map[uint32]string)},
	}

	funcs := map[string]func(client mqtt.Client, message mqtt.Message){
		"pulseaudio/sink/default/set":  bridge.onDefaultSinkSet,
		"pulseaudio/cardprofile/+/set": bridge.onCardProfileSet,
		// "pulseaudio/mute/set":          bridge.onMuteSet,
		// "pulseaudio/volume/set":        bridge.onVolumeSet,
	}
	for key, function := range funcs {
		token := mqttClient.Subscribe(key, 0, function)
		token.Wait()
	}
	time.Sleep(2 * time.Second)
	return bridge
}

var sendMutex sync.Mutex

func (bridge *PulseaudioMQTTBridge) onDefaultSinkSet(client mqtt.Client, message mqtt.Message) {
	sendMutex.Lock()
	defer sendMutex.Unlock()

	defaultSink := string(message.Payload())
	if defaultSink != "" {
		bridge.PublishMQTT("pulseaudio/sink/default/set", "", false)
		bridge.PAClient.SetDefaultSink(defaultSink)
	}
}

// Missing ability to read these, so disabled for now
// func (bridge *PulseaudioMQTTBridge) onMuteSet(client mqtt.Client, message mqtt.Message) {
// 	sendMutex.Lock()
// 	defer sendMutex.Unlock()

// 	mute, err := strconv.ParseBool(string(message.Payload()))
// 	if err != nil {
// 		fmt.Printf("Could not parse '%s' as bool\n", string(message.Payload()))
// 		return
// 	}
// 	bridge.PublishMQTT("pulseaudio/mute/set", "", false)
// 	bridge.PAClient.SetMute(mute)
// }

// func (bridge *PulseaudioMQTTBridge) onVolumeSet(client mqtt.Client, message mqtt.Message) {
// 	sendMutex.Lock()
// 	defer sendMutex.Unlock()

// 	volume, err := strconv.ParseFloat(string(message.Payload()))
// 	if err != nil {
// 		fmt.Printf("Could not parse '%s' as float\n", string(message.Payload()))
// 		return
// 	}
// 	bridge.PublishMQTT("pulseaudio/volume/set", "", false)
// 	bridge.PAClient.SetVolume(float32(volume))
// }

func (bridge *PulseaudioMQTTBridge) onCardProfileSet(client mqtt.Client, message mqtt.Message) {
	sendMutex.Lock()
	defer sendMutex.Unlock()

	re := regexp.MustCompile(`^pulseaudio/cardprofile/([^/]+)/set$`)
	matches := re.FindStringSubmatch(message.Topic())
	if matches != nil {
		cardStr := matches[1]
		card, err := strconv.ParseUint(cardStr, 10, 32)
		if err != nil {
			fmt.Printf("Could not parse card '%s'\n", cardStr)
			return
		}
		profile := string(message.Payload())
		if profile != "" {
			bridge.PublishMQTT("pulseaudio/cardprofile/"+cardStr+"/set", "", false)
			bridge.PAClient.SetCardProfile(uint32(card), profile)
		}
	} else {
		//TODO
	}
}

func (bridge *PulseaudioMQTTBridge) PublishMQTT(topic string, message string, retained bool) {
	token := bridge.MQTTClient.Publish(topic, 0, retained, message)
	token.Wait()
}

func (bridge *PulseaudioMQTTBridge) MainLoop() {
	updates, err := bridge.PAClient.Updates()
	if err != nil {
		panic(err)
	}
	for ; ; <-updates {
		info, err := bridge.PAClient.ServerInfo()
		if err != nil {
			fmt.Printf("Could not retrieve server info\n")
			continue
		}
		changeDetected := false

		if info.DefaultSink != bridge.PulseAudioState.DefaultSink {
			bridge.PulseAudioState.DefaultSink = info.DefaultSink
			changeDetected = true
		}
		cards, err := bridge.PAClient.Cards()
		if err != nil {
			fmt.Printf("Could not retrieve cards\n")
			continue
		}
		for _, card := range cards {
			value, exists := bridge.PulseAudioState.ActiveProfilePerCard[card.Index]
			if !exists || value != card.ActiveProfile.Name {
				bridge.PulseAudioState.ActiveProfilePerCard[card.Index] = card.ActiveProfile.Name
				changeDetected = true
			}
			//TODO handle removed cards?
		}
		if changeDetected {
			jsonState, err := json.Marshal(bridge.PulseAudioState)
			if err != nil {
				fmt.Printf("Could not serialize state %v\n", err)
				continue
			}
			bridge.PublishMQTT("pulseaudio/state", string(jsonState), false)
		}
	}
}

func printHelp() {
	fmt.Println("Usage: pulseaudio-mqtt [OPTIONS]")
	fmt.Println("Options:")
	flag.PrintDefaults()
}

func main() {
	mqttBroker := flag.String("broker", "tcp://localhost:1883", "MQTT broker URL")
	help := flag.Bool("help", false, "Print help")
	debug = flag.Bool("debug", false, "Debug logging")
	flag.Parse()

	if *help {
		printHelp()
		os.Exit(0)
	}

	bridge := NewPulseaudioMQTTBridge(*mqttBroker)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	fmt.Printf("Started\n")
	go bridge.MainLoop()
	<-c
	bridge.PAClient.Close()
	fmt.Printf("Shut down\n")

	os.Exit(0)
}
