package telnyx

import (
	"encoding/base64"
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/weedge/pipeline-go/pkg/frames"

	"achatbot/pkg/consts"
)

// Telnyx media streaming audio is G.711 µ-law at 8 kHz.
const telnyxRate = 8000

// echoTail is how long after the bot's audio finishes playing we keep
// suppressing inbound audio, to catch the tail of acoustic echo.
const echoTail = 250 * time.Millisecond

// mediaMessage is the Telnyx bidirectional media-streaming envelope (a subset).
type mediaMessage struct {
	Event    string `json:"event"`
	StreamID string `json:"stream_id,omitempty"`
	Media    *struct {
		Track   string `json:"track,omitempty"`
		Payload string `json:"payload"`
	} `json:"media,omitempty"`
}

// Serializer converts between Telnyx media JSON (base64 µ-law/8 kHz) and the
// pipeline's PCM16 AudioRawFrames, resampling to/from pipelineRate. It
// implements the pipeline-go serializers.Serializer interface so the existing
// transport and processors work unchanged.
type Serializer struct {
	pipelineRate int

	// Half-duplex echo suppression: while the bot's audio is still playing
	// (Telnyx buffers what we send), inbound audio is mostly the bot echoing
	// back on a phone without echo cancellation. We estimate when playback
	// ends and drop inbound until then, so the bot does not hear itself.
	mu             sync.Mutex
	playbackEndsAt time.Time
	suppressEcho   bool
}

// NewSerializer builds a Telnyx media serializer for the given pipeline sample
// rate (typically 16000), with half-duplex echo suppression enabled.
func NewSerializer(pipelineRate int) *Serializer {
	if pipelineRate == 0 {
		pipelineRate = consts.DefaultRate
	}
	return &Serializer{pipelineRate: pipelineRate, suppressEcho: true}
}

// echoActive reports whether the bot is (estimated to be) still playing.
func (s *Serializer) echoActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.playbackEndsAt.Add(echoTail))
}

// noteOutbound advances the estimated playback-end clock by the duration of a
// chunk of outbound audio (given as sample count at telnyxRate).
func (s *Serializer) noteOutbound(samples int) {
	dur := time.Duration(samples) * time.Second / telnyxRate
	s.mu.Lock()
	now := time.Now()
	if now.After(s.playbackEndsAt) {
		s.playbackEndsAt = now
	}
	s.playbackEndsAt = s.playbackEndsAt.Add(dur)
	s.mu.Unlock()
}

// resetPlayback marks playback as finished (used on interruption/clear, when
// Telnyx flushes its buffer).
func (s *Serializer) resetPlayback() {
	s.mu.Lock()
	s.playbackEndsAt = time.Now()
	s.mu.Unlock()
}

// Deserialize turns an inbound Telnyx "media" message into an AudioRawFrame at
// the pipeline rate. Non-media events (connected/start/stop) return (nil, nil)
// so the input processor skips them.
func (s *Serializer) Deserialize(data []byte) (frames.Frame, error) {
	var m mediaMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, nil // ignore anything unparseable rather than kill the read loop
	}
	if m.Event != "media" || m.Media == nil || m.Media.Payload == "" {
		return nil, nil
	}
	if s.suppressEcho && s.echoActive() {
		return nil, nil // drop inbound while the bot is still speaking (echo)
	}
	mulaw, err := base64.StdEncoding.DecodeString(m.Media.Payload)
	if err != nil {
		return nil, nil
	}
	pcm8 := MuLawToPCM16(mulaw)
	pcm := ResamplePCM16(pcm8, telnyxRate, s.pipelineRate)
	return frames.NewAudioRawFrame(pcm, s.pipelineRate, 1, 2), nil
}

// Serialize turns an outbound frame into a Telnyx message. AudioRawFrames
// (bot audio at frame.SampleRate) become media messages: resample to 8 kHz,
// µ-law encode, base64. StartInterruptionFrames become a "clear" message that
// flushes Telnyx's playback buffer, so barge-in actually stops the bot on a
// phone call (Telnyx keeps playing buffered audio otherwise).
func (s *Serializer) Serialize(frame frames.Frame) ([]byte, error) {
	switch af := frame.(type) {
	case *frames.StartInterruptionFrame:
		s.resetPlayback()
		return json.Marshal(map[string]string{"event": "clear"})
	case *frames.AudioRawFrame:
		if len(af.Audio) == 0 {
			return nil, nil
		}
		pcm8 := ResamplePCM16(af.Audio, af.SampleRate, telnyxRate)
		s.noteOutbound(len(pcm8) / 2)
		mulaw := PCM16ToMuLaw(pcm8)
		payload := base64.StdEncoding.EncodeToString(mulaw)
		out := mediaMessage{Event: "media", Media: &struct {
			Track   string `json:"track,omitempty"`
			Payload string `json:"payload"`
		}{Payload: payload}}
		return json.Marshal(out)
	default:
		return nil, nil
	}
}

// Conn adapts a gorilla WebSocket to the pipeline's IWebSocketConn. Telnyx
// sends JSON text frames; ReadMessage reports them as BinaryMessage so the
// input processor (which only handles binary) deserializes them, and
// WriteMessage always sends text (what Telnyx expects).
type Conn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

// NewConn wraps a gorilla WebSocket.
func NewConn(ws *websocket.Conn) *Conn { return &Conn{ws: ws} }

// ReadMessage reads the next Telnyx frame, reporting it as BinaryMessage.
func (c *Conn) ReadMessage() (consts.MessageType, []byte, error) {
	_, data, err := c.ws.ReadMessage()
	return consts.BinaryMessage, data, err
}

// WriteMessage sends data to Telnyx as a text frame.
func (c *Conn) WriteMessage(_ consts.MessageType, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteMessage(websocket.TextMessage, data)
}

// Close closes the underlying socket.
func (c *Conn) Close() error { return c.ws.Close() }
