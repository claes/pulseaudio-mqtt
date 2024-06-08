package lib

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/jfreymuth/pulse/proto"
)

type PulseAudioState struct {
	DefaultSink          PulseAudioSink
	DefaultSource        PulseAudioSource
	Sinks                []PulseAudioSink
	Sources              []PulseAudioSource
	Cards                []PulseAudioCard
	ActiveProfilePerCard map[uint32]string
}

type PulseAudioSink struct {
	Name string
	Id   string
}

type PulseAudioSource struct {
	Name string
	Id   string
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
	sendMutex       sync.Mutex
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

func NewPulseaudioMQTTBridge(pulseClient *PulseClient, mqttClient mqtt.Client) *PulseaudioMQTTBridge {

	bridge := &PulseaudioMQTTBridge{
		MqttClient:  mqttClient,
		PulseClient: pulseClient,
		PulseAudioState: PulseAudioState{PulseAudioSink{}, PulseAudioSource{}, []PulseAudioSink{}, []PulseAudioSource{},
			[]PulseAudioCard{}, make(map[uint32]string)},
	}

	funcs := map[string]func(client mqtt.Client, message mqtt.Message){
		"pulseaudio/sink/default/set":  bridge.onDefaultSinkSet,
		"pulseaudio/cardprofile/+/set": bridge.onCardProfileSet,
		"pulseaudio/mute/set":          bridge.onMuteSet,
		"pulseaudio/volume/set":        bridge.onVolumeSet,
		"pulseaudio/initialize":        bridge.onInitialize,
	}
	for key, function := range funcs {
		token := mqttClient.Subscribe(key, 0, function)
		token.Wait()
	}

	bridge.initialize()

	time.Sleep(2 * time.Second)
	return bridge
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

func (bridge *PulseaudioMQTTBridge) PublishMQTT(topic string, message string, retained bool) {
	token := bridge.MqttClient.Publish(topic, 0, retained, message)
	token.Wait()
}

func (bridge *PulseaudioMQTTBridge) MainLoop() {

	ch := make(chan struct{}, 1)
	bridge.PulseClient.protoClient.Callback = func(msg interface{}) {
		switch msg := msg.(type) {
		case *proto.SubscribeEvent:
			if msg.Event.GetType() == proto.EventChange {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}

	err := bridge.PulseClient.protoClient.Request(&proto.Subscribe{Mask: proto.SubscriptionMaskAll}, nil)
	if err != nil {
		slog.Error("Failed pulseclient subscription", "error", err)
		return
	}

	go func() {
		for {
			<-ch
			defaultSinkChanged, err := bridge.checkUpdateDefaultSink()
			if err != nil {
				slog.Error("Error when checking update of default sink", "error", err)
				continue
			}
			defaultSourceChanged, err := bridge.checkUpdateDefaultSource()
			if err != nil {
				slog.Error("Error when checking update of default source", "error", err)
				continue
			}
			activeProfileChanged, err := bridge.checkUpdateActiveProfile()
			if err != nil {
				slog.Error("Error when checking update of active profile", "error", err)
				continue
			}
			//check update list of sinks or list of sources
			if defaultSinkChanged || activeProfileChanged || defaultSourceChanged {
				slog.Debug("State change detected",
					"defaultSinkChanged", defaultSinkChanged,
					"defaultSourceChanged", defaultSourceChanged,
					"activeProfileChanged", activeProfileChanged)
				bridge.publishState()
			}
		}
	}()
}

func (bridge *PulseaudioMQTTBridge) publishState() {
	jsonState, err := json.Marshal(bridge.PulseAudioState)
	if err != nil {
		slog.Error("Could not serialize state", "error", err)
		return
	}
	bridge.PublishMQTT("pulseaudio/state", string(jsonState), false)
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
		s = append(s, PulseAudioSource{source.Name(), source.ID()})
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
		s = append(s, PulseAudioSink{sink.Name(), sink.ID()})
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

func (bridge *PulseaudioMQTTBridge) checkUpdateDefaultSource() (bool, error) {
	source, err := bridge.PulseClient.DefaultSource()
	if err != nil {
		slog.Error("Could not retrieve default source", "error", err)
		return false, err
	}
	changeDetected := false
	if source.ID() != bridge.PulseAudioState.DefaultSource.Id {
		bridge.PulseAudioState.DefaultSource.Name = source.Name()
		bridge.PulseAudioState.DefaultSource.Id = source.ID()
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
	changeDetected := false
	if sink.ID() != bridge.PulseAudioState.DefaultSink.Id {
		bridge.PulseAudioState.DefaultSink.Name = sink.Name()
		bridge.PulseAudioState.DefaultSink.Id = sink.ID()
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
