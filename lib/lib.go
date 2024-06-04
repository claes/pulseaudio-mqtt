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
	DefaultSink          string
	ActiveProfilePerCard map[uint32]string
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
		MqttClient:      mqttClient,
		PulseClient:     pulseClient,
		PulseAudioState: PulseAudioState{"", make(map[uint32]string)},
	}

	funcs := map[string]func(client mqtt.Client, message mqtt.Message){
		"pulseaudio/sink/default/set":  bridge.onDefaultSinkSet,
		"pulseaudio/cardprofile/+/set": bridge.onCardProfileSet,
		"pulseaudio/mute/set":          bridge.onMuteSet,
		"pulseaudio/volume/set":        bridge.onVolumeSet,
	}
	for key, function := range funcs {
		token := mqttClient.Subscribe(key, 0, function)
		token.Wait()
	}

	bridge.checkUpdateDefaultSink()
	bridge.checkUpdateActiveProfile()
	bridge.publishState()

	time.Sleep(2 * time.Second)
	return bridge
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
		panic(err)
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
			activeProfileChanged, err := bridge.checkUpdateActiveProfile()
			if err != nil {
				slog.Error("Error when checking update of active profile", "error", err)
				continue
			}
			if defaultSinkChanged || activeProfileChanged {
				slog.Debug("State change detected",
					"defaultSinkChanged", defaultSinkChanged,
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

func (bridge *PulseaudioMQTTBridge) checkUpdateDefaultSink() (bool, error) {
	sink, err := bridge.PulseClient.DefaultSink()
	if err != nil {
		slog.Error("Could not retrieve default sink", "error", err)
		return false, err
	}
	changeDetected := false
	if sink.Name() != bridge.PulseAudioState.DefaultSink {
		bridge.PulseAudioState.DefaultSink = sink.Name()
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
	for _, cardInfo := range reply {
		value, exists := bridge.PulseAudioState.ActiveProfilePerCard[cardInfo.CardIndex]
		if !exists || value != cardInfo.ActiveProfileName {
			bridge.PulseAudioState.ActiveProfilePerCard[cardInfo.CardIndex] = cardInfo.ActiveProfileName
			changeDetected = true
		}
	}
	// TODO handle removed cards?
	return changeDetected, nil
}
