package lib

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/jfreymuth/pulse/proto"
)

type PulseAudioState struct {
	DefaultSink          PulseAudioSink
	DefaultSource        PulseAudioSource
	Sinks                []PulseAudioSink
	SinkInputs           []PulseAudioSinkInput
	Clients              []PulseAudioClient
	Sources              []PulseAudioSource
	Cards                []PulseAudioCard
	ActiveProfilePerCard map[uint32]string
}

type PulseAudioSink struct {
	Name      string
	Id        string
	SinkIndex uint32
	State     uint32
	Mute      bool
}

type PulseAudioSinkInput struct {
	MediaName      string
	SinkInputIndex uint32
	ClientIndex    uint32
	SinkIndex      uint32
	Mute           bool

	Properties map[string]string
}

type PulseAudioClient struct {
	ClientIndex uint32
	Application string
	Properties  map[string]string
}

type PulseAudioSource struct {
	Name  string
	Id    string
	State uint32
	Mute  bool
}

type PulseAudioCard struct {
	Name              string
	Index             uint32
	ActiveProfileName string

	Profiles []PulseAudioProfile
	Ports    []PulseAudioPort
}

type PulseAudioProfile struct {
	Name        string
	Description string
}

type PulseAudioPort struct {
	Name        string
	Description string
}

type PulseaudioMQTTBridge struct {
	MqttClient      mqtt.Client
	PulseClient     *PulseClient
	PulseAudioState PulseAudioState
	TopicPrefix     string
	sendMutex       sync.Mutex
}

type SinkInputReq struct {
	Command        string
	SinkInputIndex uint32
	SinkName       string
}

type DetectedChanges struct {
	defaultSinkChanged,
	activeProfileChanged,
	defaultSourceChanged,
	sinksChanged,
	sinkInputsChanged,
	clientsChanged,
	cardsChanged,
	sourcesChanged bool
}

func (d DetectedChanges) String2() string {
	return fmt.Sprintf(
		"DetectedChanges{defaultSinkChanged: %v, activeProfileChanged: %v, defaultSourceChanged: %v, sinksChanged: %v, sinkInputsChanged: %v, clientsChanged: %v, cardsChanged: %v, sourcesChanged: %v}",
		d.defaultSinkChanged,
		d.activeProfileChanged,
		d.defaultSourceChanged,
		d.sinksChanged,
		d.sinkInputsChanged,
		d.clientsChanged,
		d.cardsChanged,
		d.sourcesChanged,
	)
}

func (d DetectedChanges) AnyChanged() bool {
	return d.defaultSinkChanged ||
		d.activeProfileChanged ||
		d.defaultSourceChanged ||
		d.sinksChanged ||
		d.sinkInputsChanged ||
		d.clientsChanged ||
		d.cardsChanged ||
		d.sourcesChanged
}

func CreatePulseClient(pulseServer string) (*PulseClient, error) {
	pulseClient, err := NewPulseClient(ClientServerString(pulseServer))
	if err != nil {
		slog.Error("Error while initializing pulseclient", "pulseServer", pulseServer)
		return nil, err
	} else {
		slog.Info("Initialized pulseclient", "pulseServer", pulseServer)
	}
	return pulseClient, nil
}

func CreateMQTTClient(mqttBroker string) (mqtt.Client, error) {
	slog.Info("Creating MQTT client", "broker", mqttBroker)
	opts := mqtt.NewClientOptions().AddBroker(mqttBroker)
	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		slog.Error("Could not connect to broker", "mqttBroker", mqttBroker, "error", token.Error())
		return nil, token.Error()
	}
	slog.Info("Connected to MQTT broker", "mqttBroker", mqttBroker)
	return client, nil
}

func NewPulseaudioMQTTBridge(pulseClient *PulseClient, mqttClient mqtt.Client, topicPrefix string) *PulseaudioMQTTBridge {

	bridge := &PulseaudioMQTTBridge{
		MqttClient:  mqttClient,
		PulseClient: pulseClient,
		TopicPrefix: topicPrefix,
		PulseAudioState: PulseAudioState{
			PulseAudioSink{},
			PulseAudioSource{},
			[]PulseAudioSink{},
			[]PulseAudioSinkInput{},
			[]PulseAudioClient{},
			[]PulseAudioSource{},
			[]PulseAudioCard{},
			make(map[uint32]string)},
	}

	funcs := map[string]func(client mqtt.Client, message mqtt.Message){
		"pulseaudio/sink/default/set":  bridge.onDefaultSinkSet,
		"pulseaudio/cardprofile/+/set": bridge.onCardProfileSet,
		"pulseaudio/mute/set":          bridge.onMuteSet,
		"pulseaudio/volume/set":        bridge.onVolumeSet,
		"pulseaudio/initialize":        bridge.onInitialize,
		"pulseaudio/sinkinput/req":     bridge.onSinkInputReq,
	}
	for key, function := range funcs {
		token := mqttClient.Subscribe(prefixify(topicPrefix, key), 0, function)
		token.Wait()
	}

	bridge.initialize()

	time.Sleep(2 * time.Second)
	return bridge
}

func prefixify(topicPrefix, subtopic string) string {
	if len(strings.TrimSpace(topicPrefix)) > 0 {
		return topicPrefix + "/" + subtopic
	} else {
		return subtopic
	}
}

func (bridge *PulseaudioMQTTBridge) onInitialize(client mqtt.Client, message mqtt.Message) {
	command := string(message.Payload())
	if command != "" {
		bridge.PublishMQTT("pulseaudio/initialize", "", false)
		bridge.initialize()
	}
}

func (bridge *PulseaudioMQTTBridge) initialize() {
	bridge.checkUpdateSources()
	bridge.checkUpdateSinks()
	bridge.checkUpdateSinkInputs()
	bridge.checkUpdateClients()
	bridge.checkUpdateDefaultSink()
	bridge.checkUpdateDefaultSource()
	bridge.checkUpdateActiveProfile()
	bridge.publishState()
}

func (bridge *PulseaudioMQTTBridge) onDefaultSinkSet(client mqtt.Client, message mqtt.Message) {
	bridge.sendMutex.Lock()
	defer bridge.sendMutex.Unlock()

	defaultSink := string(message.Payload())
	if defaultSink != "" {
		bridge.PublishMQTT("pulseaudio/sink/default/set", "", false)
		bridge.PulseClient.protoClient.Request(&proto.SetDefaultSink{SinkName: defaultSink}, nil)
	}
}

func (bridge *PulseaudioMQTTBridge) onSinkInputReq(client mqtt.Client, message mqtt.Message) {
	bridge.sendMutex.Lock()
	defer bridge.sendMutex.Unlock()

	if len(message.Payload()) > 0 {
		var sinkInputReq SinkInputReq
		err := json.Unmarshal(message.Payload(), &sinkInputReq)
		if err != nil {
			slog.Error("Error unmarshaling sink input command", "error", err, "payload", string(message.Payload()))
			return
		}

		if strings.EqualFold(sinkInputReq.Command, "movesink") {
			sinkName := string(message.Payload())
			if sinkName != "" {
				bridge.PublishMQTT("pulseaudio/sinkinput/req", "", false)
				err := bridge.PulseClient.protoClient.Request(&proto.MoveSinkInput{
					SinkInputIndex: sinkInputReq.SinkInputIndex, DeviceIndex: proto.Undefined, DeviceName: sinkInputReq.SinkName}, nil)

				if err != nil {
					slog.Error("Could not set card profile", "error", err)
					return
				}
			}
		}
	}
}

func (bridge *PulseaudioMQTTBridge) onMuteSet(client mqtt.Client, message mqtt.Message) {
	bridge.sendMutex.Lock()
	defer bridge.sendMutex.Unlock()

	mute, err := strconv.ParseBool(string(message.Payload()))
	if err != nil {
		slog.Error("Could not parse bool", "messagePayload", message.Payload())
	}
	bridge.PublishMQTT("pulseaudio/mute/set", "", false)
	sink, err := bridge.PulseClient.DefaultSink()
	if err != nil {
		slog.Error("Could not retrieve default sink", "error", err)
	}
	err = bridge.PulseClient.protoClient.Request(&proto.SetSinkMute{SinkIndex: sink.SinkIndex(), Mute: mute}, nil)
	if err != nil {
		slog.Error("Could not mute sink", "error", err, "mute", mute, "sink", sink.info.SinkIndex)
	}
}

// See https://github.com/jfreymuth/pulse/pull/8/files
func (bridge *PulseaudioMQTTBridge) onVolumeSet(client mqtt.Client, message mqtt.Message) {
	bridge.sendMutex.Lock()
	defer bridge.sendMutex.Unlock()

	volume, err := strconv.ParseFloat(string(message.Payload()), 32)
	if err != nil {
		slog.Error("Could not parse float", "payload", message.Payload())
		return
	}
	bridge.PublishMQTT("pulseaudio/volume/set", "", false)

	sink, err := bridge.PulseClient.DefaultSink()
	if err != nil {
		slog.Error("Could not retrieve default sink", "error", err)
		return
	}

	err = bridge.PulseClient.SetSinkVolume(sink, float32(volume))
	if err != nil {
		slog.Error("Could not set card profile", "error", err)
		return
	}
}

func (bridge *PulseaudioMQTTBridge) onCardProfileSet(client mqtt.Client, message mqtt.Message) {
	bridge.sendMutex.Lock()
	defer bridge.sendMutex.Unlock()

	re := regexp.MustCompile(`^pulseaudio/cardprofile/([^/]+)/set$`)
	matches := re.FindStringSubmatch(message.Topic())
	if matches != nil {
		cardStr := matches[1]
		card, err := strconv.ParseUint(cardStr, 10, 32)
		if err != nil {
			slog.Error("Could not parse card", "card", cardStr)
			return
		}

		profile := string(message.Payload())
		if profile != "" {
			bridge.PublishMQTT("pulseaudio/cardprofile/"+cardStr+"/set", "", false)
			err = bridge.PulseClient.protoClient.Request(&proto.SetCardProfile{CardIndex: uint32(card), ProfileName: profile}, nil)
			if err != nil {
				slog.Error("Could not set card profile", "error", err)
				return
			}
		}
	} else {
		//TODO
	}
}

func (bridge *PulseaudioMQTTBridge) PublishMQTT(subtopic string, message string, retained bool) {
	token := bridge.MqttClient.Publish(prefixify(bridge.TopicPrefix, subtopic), 0, retained, message)
	token.Wait()
}

func (bridge *PulseaudioMQTTBridge) MainLoop() {

	eventChannels := map[proto.SubscriptionEventType]chan proto.SubscriptionEventType{
		proto.EventChange: make(chan proto.SubscriptionEventType, 1),
		proto.EventNew:    make(chan proto.SubscriptionEventType, 1),
		proto.EventRemove: make(chan proto.SubscriptionEventType, 1),
	}

	bridge.PulseClient.protoClient.Callback = func(msg interface{}) {

		switch msg := msg.(type) {
		case *proto.SubscribeEvent:
			if ch, ok := eventChannels[msg.Event.GetType()]; ok {
				select {
				case ch <- msg.Event:
				default:
				}
			}
		default:
			slog.Info("Pulse unknown event received", "evt", msg)
		}
	}

	err := bridge.PulseClient.protoClient.Request(&proto.Subscribe{Mask: proto.SubscriptionMaskAll}, nil)
	if err != nil {
		slog.Error("Failed pulseclient subscription", "error", err)
		return
	}

	go func() {
		for {
			select {
			case event := <-eventChannels[proto.EventNew]:
				slog.Debug("Event new", "event", event)
			case event := <-eventChannels[proto.EventRemove]:
				slog.Debug("Event remove", "event", event)
			case event := <-eventChannels[proto.EventChange]:
				slog.Info("Event change", "event", event)

				var err error
				c := DetectedChanges{}

				switch event.GetFacility() {
				case proto.EventSink:
					c.defaultSinkChanged, err = bridge.checkUpdateDefaultSink()
					c.sinksChanged, err = bridge.checkUpdateSinks()
				case proto.EventSource:
					c.defaultSourceChanged, err = bridge.checkUpdateDefaultSource()
					c.activeProfileChanged, err = bridge.checkUpdateActiveProfile()
					c.sourcesChanged, err = bridge.checkUpdateSources()
				case proto.EventSinkSinkInput:
					c.defaultSinkChanged, err = bridge.checkUpdateDefaultSink()
					c.sinkInputsChanged, err = bridge.checkUpdateSinkInputs()
				case proto.EventClient:
					c.clientsChanged, err = bridge.checkUpdateClients()
				case proto.EventCard:
					c.cardsChanged, err = bridge.checkUpdateActiveProfile()
				case proto.EventSinkSourceOutput:
				case proto.EventModule:
				case proto.EventServer:
				}

				if err != nil {
					slog.Error("Error when checking event", "error", err, "event", event)
					continue
				}

				slog.Info("Change detection outcome",
					"changeDetection", c)

				if c.AnyChanged() {
					slog.Info("State change detected")
					bridge.publishState()
				} else {
					slog.Info("No state change detected")
				}
			}
		}
	}()
}

func (bridge *PulseaudioMQTTBridge) publishState() {
	bridge.publishStateGranular(DetectedChanges{
		defaultSinkChanged:   true,
		activeProfileChanged: true,
		defaultSourceChanged: true,
		sinksChanged:         true,
		sinkInputsChanged:    true,
		clientsChanged:       true,
		cardsChanged:         true,
		sourcesChanged:       true,
	})
}

func (bridge *PulseaudioMQTTBridge) publishStateGranular(c DetectedChanges) {
	jsonState, err := json.Marshal(bridge.PulseAudioState)
	if err != nil {
		slog.Error("Could not serialize state", "error", err)
		return
	}
	//TODO remove?
	bridge.PublishMQTT("pulseaudio/state", string(jsonState), false)

	if c.defaultSinkChanged || c.sinkInputsChanged {
		jsonState, err = json.Marshal(bridge.PulseAudioState.DefaultSink)
		if err != nil {
			slog.Error("Could not serialize state", "error", err)
			return
		}
		bridge.PublishMQTT("pulseaudio/defaultsink", string(jsonState), false)
	}

	if c.defaultSourceChanged {
		jsonState, err = json.Marshal(bridge.PulseAudioState.DefaultSource)
		if err != nil {
			slog.Error("Could not serialize state", "error", err)
			return
		}
		bridge.PublishMQTT("pulseaudio/defaultsource", string(jsonState), false)
	}

	if c.activeProfileChanged {
		jsonState, err = json.Marshal(bridge.PulseAudioState.ActiveProfilePerCard)
		if err != nil {
			slog.Error("Could not serialize state", "error", err)
			return
		}
		bridge.PublishMQTT("pulseaudio/activeprofilepercard", string(jsonState), false)
	}

	if c.clientsChanged {
		jsonState, err = json.Marshal(bridge.PulseAudioState.Clients)
		if err != nil {
			slog.Error("Could not serialize state", "error", err)
			return
		}
		bridge.PublishMQTT("pulseaudio/clients", string(jsonState), false)
	}

	if c.sinkInputsChanged {
		jsonState, err = json.Marshal(bridge.PulseAudioState.SinkInputs)
		if err != nil {
			slog.Error("Could not serialize state", "error", err)
			return
		}
		bridge.PublishMQTT("pulseaudio/sinkinputs", string(jsonState), false)
	}

	if c.sinksChanged {
		jsonState, err = json.Marshal(bridge.PulseAudioState.Sinks)
		if err != nil {
			slog.Error("Could not serialize state", "error", err)
			return
		}
		bridge.PublishMQTT("pulseaudio/sinks", string(jsonState), false)
	}
	if c.sourcesChanged {
		jsonState, err = json.Marshal(bridge.PulseAudioState.Sources)
		if err != nil {
			slog.Error("Could not serialize state", "error", err)
			return
		}
		bridge.PublishMQTT("pulseaudio/sources", string(jsonState), false)
	}
	if c.cardsChanged {
		jsonState, err = json.Marshal(bridge.PulseAudioState.Cards)
		if err != nil {
			slog.Error("Could not serialize state", "error", err)
			return
		}
		bridge.PublishMQTT("pulseaudio/cards", string(jsonState), false)
	}
}

func (bridge *PulseaudioMQTTBridge) checkUpdateSources() (bool, error) {
	sources, err := bridge.PulseClient.ListSources()
	if err != nil {
		slog.Error("Could not retrieve sources", "error", err)
		return false, err
	}
	changeDetected := false
	var s []PulseAudioSource
	for _, source := range sources {
		s = append(s, PulseAudioSource{source.Name(), source.ID(), source.State(), source.Mute()})
	}
	if len(s) != len(bridge.PulseAudioState.Sources) {
		changeDetected = true
	} else {
		for i := range s {
			if s[i] != bridge.PulseAudioState.Sources[i] {
				changeDetected = true
				break
			}
		}
	}
	bridge.PulseAudioState.Sources = s
	return changeDetected, nil
}

func (bridge *PulseaudioMQTTBridge) checkUpdateSinks() (bool, error) {
	sinks, err := bridge.PulseClient.ListSinks()
	if err != nil {
		slog.Error("Could not retrieve sinks", "error", err)
		return false, err
	}
	changeDetected := false
	var s []PulseAudioSink
	for _, sink := range sinks {
		s = append(s, PulseAudioSink{sink.Name(), sink.ID(), sink.SinkIndex(), sink.State(), sink.Mute()})
	}

	if len(s) != len(bridge.PulseAudioState.Sinks) {
		changeDetected = true
	} else {
		for i := range s {
			if s[i] != bridge.PulseAudioState.Sinks[i] {
				changeDetected = true
				break
			}
		}
	}
	bridge.PulseAudioState.Sinks = s
	return changeDetected, nil
}

func (bridge *PulseaudioMQTTBridge) checkUpdateSinkInputs() (bool, error) {
	sinkInputs, err := bridge.PulseClient.ListSinkInputs()
	if err != nil {
		slog.Error("Could not retrieve sink inputs", "error", err)
		return false, err
	}
	changeDetected := false
	var s []PulseAudioSinkInput
	for _, sinkInput := range sinkInputs {
		props := make(map[string]string)
		for key, value := range sinkInput.info.Properties {
			props[key] = strings.TrimRight(string(value), "\u0000")
		}
		s = append(s, PulseAudioSinkInput{
			MediaName:      sinkInput.info.MediaName,
			ClientIndex:    sinkInput.info.ClientIndex,
			SinkInputIndex: sinkInput.info.SinkInputIndex,
			SinkIndex:      sinkInput.info.SinkIndex,
			Mute:           sinkInput.info.Muted,
			Properties:     props})
	}

	if len(s) != len(bridge.PulseAudioState.SinkInputs) {
		changeDetected = true
	} else {
		for i := range s {
			if !s[i].Equals(&bridge.PulseAudioState.SinkInputs[i]) {
				changeDetected = true
				break
			}
		}
	}
	bridge.PulseAudioState.SinkInputs = s
	return changeDetected, nil
}

func (bridge *PulseaudioMQTTBridge) checkUpdateClients() (bool, error) {
	clients, err := bridge.PulseClient.ListClients()
	if err != nil {
		slog.Error("Could not retrieve clients", "error", err)
		return false, err
	}
	changeDetected := false
	var c []PulseAudioClient
	for _, client := range clients {
		props := make(map[string]string)
		for key, value := range client.info.Properties {
			props[key] = strings.TrimRight(string(value), "\u0000")
		}
		c = append(c, PulseAudioClient{
			ClientIndex: client.info.ClientIndex,
			Application: client.info.Application,
			Properties:  props})
	}

	if len(c) != len(bridge.PulseAudioState.Clients) {
		changeDetected = true
	} else {
		for i := range c {
			if !c[i].Equals(&bridge.PulseAudioState.Clients[i]) {
				changeDetected = true
				break
			}
		}
	}
	bridge.PulseAudioState.Clients = c
	return changeDetected, nil
}

func (bridge *PulseaudioMQTTBridge) checkUpdateDefaultSource() (bool, error) {
	source, err := bridge.PulseClient.DefaultSource()
	if err != nil {
		slog.Error("Could not retrieve default source", "error", err)
		return false, err
	}

	defaultSource := PulseAudioSource{source.Name(), source.ID(), source.State(), source.Mute()}
	changeDetected := false
	if defaultSource != bridge.PulseAudioState.DefaultSource {
		bridge.PulseAudioState.DefaultSource = defaultSource
		changeDetected = true
	}
	return changeDetected, nil
}

func (bridge *PulseaudioMQTTBridge) checkUpdateDefaultSink() (bool, error) {
	sink, err := bridge.PulseClient.DefaultSink()
	if err != nil {
		slog.Error("Could not retrieve default sink", "error", err)
		return false, err
	}

	defaultSink := PulseAudioSink{sink.Name(), sink.ID(), sink.SinkIndex(), sink.State(), sink.Mute()}
	changeDetected := false
	if defaultSink != bridge.PulseAudioState.DefaultSink {
		bridge.PulseAudioState.DefaultSink = defaultSink
		changeDetected = true
	}
	return changeDetected, nil
}

func (bridge *PulseaudioMQTTBridge) checkUpdateActiveProfile() (bool, error) {
	reply := proto.GetCardInfoListReply{}
	err := bridge.PulseClient.protoClient.Request(&proto.GetCardInfoList{}, &reply)
	if err != nil {
		slog.Error("Could not retrieve card list", "error", err)
		return false, err
	}
	changeDetected := false
	cards := make([]PulseAudioCard, 0)
	for _, cardInfo := range reply {
		card := PulseAudioCard{}
		card.Name = cardInfo.CardName
		card.Index = cardInfo.CardIndex
		card.ActiveProfileName = cardInfo.ActiveProfileName
		for _, profile := range cardInfo.Profiles {
			card.Profiles = append(card.Profiles, PulseAudioProfile{profile.Name, profile.Description})
		}
		for _, port := range cardInfo.Ports {
			card.Ports = append(card.Ports, PulseAudioPort{port.Name, port.Description})
		}
		cards = append(cards, card)

		value, exists := bridge.PulseAudioState.ActiveProfilePerCard[cardInfo.CardIndex]
		if !exists || value != cardInfo.ActiveProfileName {
			bridge.PulseAudioState.ActiveProfilePerCard[cardInfo.CardIndex] = cardInfo.ActiveProfileName
			changeDetected = true
		}
	}
	bridge.PulseAudioState.Cards = cards

	// TODO handle removed cards?
	return changeDetected, nil
}

func (p *PulseAudioSinkInput) Equals(other *PulseAudioSinkInput) bool {
	if p == nil || other == nil {
		return p == other
	}
	if p.MediaName != other.MediaName ||
		p.SinkInputIndex != other.SinkInputIndex ||
		p.ClientIndex != other.ClientIndex ||
		p.SinkIndex != other.SinkIndex {
		return false
	}
	if len(p.Properties) != len(other.Properties) {
		return false
	}
	for key, value := range p.Properties {
		if otherValue, exists := other.Properties[key]; !exists || otherValue != value {
			return false
		}
	}
	return true
}

func (c *PulseAudioClient) Equals(other *PulseAudioClient) bool {
	if c == nil || other == nil {
		return c == other // Both are nil, return true; otherwise false
	}

	if c.ClientIndex != other.ClientIndex || c.Application != other.Application {
		return false
	}

	// Compare the Properties maps
	if len(c.Properties) != len(other.Properties) {
		return false
	}

	for key, value := range c.Properties {
		if otherValue, exists := other.Properties[key]; !exists || otherValue != value {
			return false
		}
	}

	return true
}
