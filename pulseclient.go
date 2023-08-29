package main

import (
	"fmt"
	"net"
	"os"
	"path"
	"sync"

	// Note depedency on commit 75628dabd933dc15bd44e6945e5ef93723937388
	// which is not officially released
	"github.com/jfreymuth/pulse/proto"
)

// This Pulseaudio client adapted from github.com/jfreymuth/pulse

type PulseClient struct {
	connection  net.Conn
	protoClient *proto.Client

	mu sync.Mutex

	server string
	props  proto.PropList
}

func NewPulseClient(opts ...ClientOption) (*PulseClient, error) {
	pulseClient := &PulseClient{
		props: proto.PropList{
			"media.name":                 proto.PropListString("pulseaudio-mqtt"),
			"application.name":           proto.PropListString(path.Base(os.Args[0])),
			"application.icon_name":      proto.PropListString("audio-x-generic"),
			"application.process.id":     proto.PropListString(fmt.Sprintf("%d", os.Getpid())),
			"application.process.binary": proto.PropListString(os.Args[0]),
		},
	}
	for _, opt := range opts {
		opt(pulseClient)
	}

	var err error
	pulseClient.protoClient, pulseClient.connection, err = proto.Connect(pulseClient.server)
	if err != nil {
		return nil, err
	}

	err = pulseClient.protoClient.Request(&proto.SetClientName{Props: pulseClient.props}, &proto.SetClientNameReply{})
	if err != nil {
		pulseClient.connection.Close()
		return nil, err
	}
	return pulseClient, nil
}

func (c *PulseClient) Close() {
	c.connection.Close()
}

// A ClientOption supplies configuration when creating the client.
type ClientOption func(*PulseClient)

// ClientServerString will override the default server strings.
// Server strings are used to connect to the server. For the server string format see
// https://www.freedesktop.org/wiki/Software/PulseAudio/Documentation/User/ServerStrings/
func ClientServerString(s string) ClientOption {
	return func(c *PulseClient) { c.server = s }
}

// RawRequest can be used to send arbitrary requests.
//
// req should be one of the request types defined by the proto package.
//
// rpl must be a pointer to the correct reply type or nil. This funcion will panic if rpl has the wrong type.
//
// The returned error can be compared against errors defined by the proto package to check for specific errors.
//
// The function will always block until the server has replied, even if rpl is nil.
func (c *PulseClient) RawRequest(req proto.RequestArgs, rpl proto.Reply) error {
	return c.protoClient.Request(req, rpl)
}

// ErrConnectionClosed is a special error value indicating that the server closed the connection.
const ErrConnectionClosed = pulseError("pulseaudio: connection closed")

type pulseError string

func (e pulseError) Error() string { return string(e) }

// A Sink is an output device.
type Sink struct {
	info proto.GetSinkInfoReply
}

// ListSinks returns a list of all available output devices.
func (c *PulseClient) ListSinks() ([]*Sink, error) {
	var reply proto.GetSinkInfoListReply
	err := c.protoClient.Request(&proto.GetSinkInfoList{}, &reply)
	if err != nil {
		return nil, err
	}
	sinks := make([]*Sink, len(reply))
	for i := range sinks {
		sinks[i] = &Sink{info: *reply[i]}
	}
	return sinks, nil
}

// DefaultSink returns the default output device.
func (c *PulseClient) DefaultSink() (*Sink, error) {
	var sink Sink
	err := c.protoClient.Request(&proto.GetSinkInfo{SinkIndex: proto.Undefined}, &sink.info)
	if err != nil {
		return nil, err
	}
	return &sink, nil
}

// SinkByID looks up a sink id.
func (c *PulseClient) SinkByID(name string) (*Sink, error) {
	var sink Sink
	err := c.protoClient.Request(&proto.GetSinkInfo{SinkIndex: proto.Undefined, SinkName: name}, &sink.info)
	if err != nil {
		return nil, err
	}
	return &sink, nil
}

// ID returns the sink name. Sink names are unique identifiers, but not necessarily human-readable.
func (s *Sink) ID() string {
	return s.info.SinkName
}

// Name is a human-readable name describing the sink.
func (s *Sink) Name() string {
	return s.info.Device
}

// Channels returns the default channel map.
func (s *Sink) Channels() proto.ChannelMap {
	return s.info.ChannelMap
}

// SampleRate returns the default sample rate.
func (s *Sink) SampleRate() int {
	return int(s.info.Rate)
}

// SinkIndex returns the sink index.
// This should only be used together with (*Cient).RawRequest.
func (s *Sink) SinkIndex() uint32 {
	return s.info.SinkIndex
}

// A Source is an input device.
type Source struct {
	info proto.GetSourceInfoReply
}

// ListSources returns a list of all available input devices.
func (c *PulseClient) ListSources() ([]*Source, error) {
	var reply proto.GetSourceInfoListReply
	err := c.protoClient.Request(&proto.GetSourceInfoList{}, &reply)
	if err != nil {
		return nil, err
	}
	sinks := make([]*Source, len(reply))
	for i := range sinks {
		sinks[i] = &Source{info: *reply[i]}
	}
	return sinks, nil
}

// DefaultSource returns the default input device.
func (c *PulseClient) DefaultSource() (*Source, error) {
	var source Source
	err := c.protoClient.Request(&proto.GetSourceInfo{SourceIndex: proto.Undefined}, &source.info)
	if err != nil {
		return nil, err
	}
	return &source, nil
}

// SourceByID looks up a source id.
func (c *PulseClient) SourceByID(name string) (*Source, error) {
	var source Source
	err := c.protoClient.Request(&proto.GetSourceInfo{SourceIndex: proto.Undefined, SourceName: name}, &source.info)
	if err != nil {
		return nil, err
	}
	return &source, nil
}

// ID returns the source name. Source names are unique identifiers, but not necessarily human-readable.
func (s *Source) ID() string {
	return s.info.SourceName
}

// Name is a human-readable name describing the source.
func (s *Source) Name() string {
	return s.info.Device
}

// Channels returns the default channel map.
func (s *Source) Channels() proto.ChannelMap {
	return s.info.ChannelMap
}

// SampleRate returns the default sample rate.
func (s *Source) SampleRate() int {
	return int(s.info.Rate)
}

// SourceIndex returns the source index.
// This should only be used together with (*Cient).RawRequest.
func (s *Source) SourceIndex() uint32 {
	return s.info.SourceIndex
}

type Card struct {
	info proto.GetCardInfoReply
}

func (c *PulseClient) ListCards() ([]*Card, error) {
	var reply proto.GetCardInfoListReply // proto.GetSinkInfoListReply

	err := c.protoClient.Request(&proto.GetCardInfoList{}, &reply)
	if err != nil {
		return nil, err
	}
	cards := make([]*Card, len(reply))
	for i := range cards {
		cards[i] = &Card{info: *reply[i]}
	}
	return cards, nil
}

type SetCardProfile struct{}

func (*SetCardProfile) command() uint32 { return proto.OpSetCardProfile }
