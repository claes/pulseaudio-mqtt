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
	"github.com/jfreymuth/pulse/proto"
	// "github.com/the-jonsey/pulseaudio"
)

var debug *bool

type PAState struct {
	DefaultSink          string
	ActiveProfilePerCard map[uint32]string
}

type PulseaudioMQTTBridge struct {
	MQTTClient mqtt.Client
	//	PAClient        pulseaudio.Client
	PulseAudioState PAState
	PulseClient     PulseClient
	sendMutex       sync.Mutex
}

func NewPulseaudioMQTTBridge(pulseServer string, mqttBroker string) *PulseaudioMQTTBridge {

	opts := mqtt.NewClientOptions().AddBroker(mqttBroker)
	mqttClient := mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		panic(token.Error())
	} else if *debug {
		fmt.Printf("Connected to MQTT broker: %s\n", mqttBroker)
	}

	pulseClient, err := NewPulseClient(ClientServerString(pulseServer))
	if err != nil {
		panic(err)
	}

	sinks, err := pulseClient.ListSinks()
	for _, sink := range sinks {
		fmt.Printf("Sink: %v\n", sink)
	}

	cards, err := pulseClient.ListCards()
	for _, card := range cards {
		fmt.Printf("Card: %v %v %s\n\n", card.info.CardIndex, card.info.CardName, card.info.ActiveProfileName)
		for _, profile := range card.info.Profiles {
			fmt.Printf("Profile: %v \n\n", profile)
		}
	}

	// paClient, err := func(pulseServer string) (*pulseaudio.Client, error) {
	// 	if pulseServer == "" {
	// 		return pulseaudio.NewClient()
	// 	} else {
	// 		fmt.Println("Attempting to connect using tcp")
	// 		return pulseaudio.NewClient()
	// 		//return pulseaudio.NewClient(pulseServer)
	// 	}
	// }(pulseServer)

	if err != nil {
		panic(err)
	}

	bridge := &PulseaudioMQTTBridge{
		MQTTClient: mqttClient,
		// PAClient:        *paClient,
		PulseClient:     *pulseClient,
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

func (bridge *PulseaudioMQTTBridge) onDefaultSinkSet(client mqtt.Client, message mqtt.Message) {
	bridge.sendMutex.Lock()
	defer bridge.sendMutex.Unlock()

	defaultSink := string(message.Payload())
	if defaultSink != "" {
		bridge.PublishMQTT("pulseaudio/sink/default/set", "", false)
		//bridge.PAClient.SetDefaultSink(defaultSink)
		bridge.PulseClient.protoClient.Request(&proto.SetDefaultSink{SinkName: defaultSink}, nil)
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
	bridge.sendMutex.Lock()
	defer bridge.sendMutex.Unlock()

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
			//bridge.PAClient.SetCardProfile(uint32(card), profile)
			bridge.PulseClient.protoClient.Request(&proto.SetCardProfile{CardIndex: uint32(card), ProfileName: profile}, nil)
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

	sinkChannel := make(chan struct{}, 1)
	cardChannel := make(chan struct{}, 1)

	bridge.PulseClient.protoClient.Callback = func(msg interface{}) {
		switch msg := msg.(type) {
		case *proto.SubscribeEvent:
			fmt.Printf("Callback event. Type: %v Facility: %v\n", msg.Event.GetType(), msg.Event.GetFacility())
			//log.Printf("%s index=%d", msg.Event, msg.Index)
			// if msg.Event.GetType() == proto.EventChange && msg.Event.GetFacility() == proto.EventCard {
			// 	select {
			// 	case cardChannel <- struct{}{}:
			// 	default:
			// 	}
			// }

			// if msg.Event.GetType() == proto.EventChange && msg.Event.GetFacility() == proto.EventSink {
			// 	select {
			// 	case sinkChannel <- struct{}{}:
			// 	default:
			// 	}
			// }

			if msg.Event.GetType() == proto.EventChange {
				switch msg.Event.GetFacility() {
				case proto.EventSink:
					fmt.Println("Event sink")
					select {
					case sinkChannel <- struct{}{}:
					default:
					}
				case proto.EventCard:
					fmt.Println("Event card")
					select {
					case cardChannel <- struct{}{}:
					default:
					}
				}
			}
		default:
			fmt.Printf("%#v\n", msg)
		}
	}

	err := bridge.PulseClient.protoClient.Request(&proto.Subscribe{Mask: proto.SubscriptionMaskAll}, nil)
	if err != nil {
		panic(err)
	}

	go func() {
		for {
			<-cardChannel
			bridge.checkUpdateActiveProfile()
		}
	}()

	go func() {
		for {
			<-sinkChannel
			bridge.checkUpdateDefaultSink()
		}
	}()

	// updates, err := bridge.PAClient.Updates()
	// if err != nil {
	// 	panic(err)
	// }
	// for ; ; <-updates {
	// 	info, err := bridge.PAClient.ServerInfo()
	// 	if err != nil {
	// 		fmt.Printf("Could not retrieve server info\n")
	// 		continue
	// 	}
	// 	changeDetected := false

	// 	if info.DefaultSink != bridge.PulseAudioState.DefaultSink {
	// 		bridge.PulseAudioState.DefaultSink = info.DefaultSink
	// 		changeDetected = true
	// 	}
	// 	cards, err := bridge.PAClient.Cards()
	// 	if err != nil {
	// 		fmt.Printf("Could not retrieve cards\n")
	// 		continue
	// 	}
	// for _, card := range cards {
	// 	value, exists := bridge.PulseAudioState.ActiveProfilePerCard[card.Index]
	// 	if !exists || value != card.ActiveProfile.Name {
	// 		bridge.PulseAudioState.ActiveProfilePerCard[card.Index] = card.ActiveProfile.Name
	// 		changeDetected = true
	// 	}
	// 	//TODO handle removed cards?
	// }
	// 	if changeDetected {
	// 		jsonState, err := json.Marshal(bridge.PulseAudioState)
	// 		if err != nil {
	// 			fmt.Printf("Could not serialize state %v\n", err)
	// 			continue
	// 		}
	// 		bridge.PublishMQTT("pulseaudio/state", string(jsonState), false)
	// 	}
	// }
}

func (bridge *PulseaudioMQTTBridge) checkUpdateDefaultSink() {
	sink, err := bridge.PulseClient.DefaultSink()
	if err != nil {
		panic(err)
	}
	changeDetected := false
	if sink.Name() != bridge.PulseAudioState.DefaultSink {
		bridge.PulseAudioState.DefaultSink = sink.Name()
		changeDetected = true
	}
	if changeDetected {
		jsonState, err := json.Marshal(bridge.PulseAudioState)
		if err != nil {
			fmt.Printf("Could not serialize state %v\n", err)
			return
		}
		bridge.PublishMQTT("pulseaudio/state", string(jsonState), false)
	}
}

func (bridge *PulseaudioMQTTBridge) checkUpdateActiveProfile() {
	reply := proto.GetCardInfoListReply{}
	err := bridge.PulseClient.protoClient.Request(&proto.GetCardInfoList{}, &reply)
	if err != nil {
		panic(err)
	}
	changeDetected := false
	for i, cardInfo := range reply {
		value, exists := bridge.PulseAudioState.ActiveProfilePerCard[cardInfo.CardIndex]
		if !exists || value != cardInfo.ActiveProfileName {
			fmt.Printf("Active profile name for card %d is %s\n", i, cardInfo.ActiveProfileName)
			bridge.PulseAudioState.ActiveProfilePerCard[cardInfo.CardIndex] = cardInfo.ActiveProfileName
			changeDetected = true
		}
	}
	if changeDetected {
		jsonState, err := json.Marshal(bridge.PulseAudioState)
		if err != nil {
			fmt.Printf("Could not serialize state %v\n", err)
			return
		}
		bridge.PublishMQTT("pulseaudio/state", string(jsonState), false)
	}
}

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

	bridge := NewPulseaudioMQTTBridge(*pulseServer, *mqttBroker)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	fmt.Printf("Started\n")
	go bridge.MainLoop()
	<-c
	//bridge.PAClient.Close()
	bridge.PulseClient.Close()
	fmt.Printf("Shut down\n")

	os.Exit(0)
}
