package telnyx

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/weedge/pipeline-go/pkg/frames"

	"achatbot/pkg/consts"
)

// Telnyx media streaming audio is G.711 µ-law at 8 kHz.
const telnyxRate = 8000

// echoTail is how long after the bot's audio finishes playing we keep the
// echo gate active, to catch the tail of acoustic echo.
const echoTail = 150 * time.Millisecond

// Adaptive echo-gate tuning. While the bot is speaking, inbound audio is
// mostly its own echo at a low, stable level; the caller interrupting is
// clearly louder. We track the echo floor and let audio through only when it
// exceeds both an absolute floor and a multiple of that echo level.
const (
	bargeAbsFloor = 900.0 // min inbound RMS (int16) to count as speech at all
	bargeFactor   = 2.6   // inbound must exceed echoFloor * this to be barge-in
	echoFloorEMA  = 0.15  // smoothing for the running echo-floor estimate
)

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
	echoFloor      float64 // running estimate of inbound echo RMS during bot speech

	// Wire-level response-latency measurement: time from the caller's last
	// speech frame to the bot's first reply audio.
	lastSpeechAt time.Time
	awaitingBot  bool
	onLatency    func(time.Duration)
}

// SetLatencyHook registers a callback fired once per turn with the measured
// time from the caller's last speech to the bot's first reply audio.
func (s *Serializer) SetLatencyHook(fn func(time.Duration)) { s.onLatency = fn }

// NewSerializer builds a Telnyx media serializer for the given pipeline sample
// rate (typically 16000), with half-duplex echo suppression enabled.
func NewSerializer(pipelineRate int) *Serializer {
	if pipelineRate == 0 {
		pipelineRate = consts.DefaultRate
	}
	return &Serializer{pipelineRate: pipelineRate, suppressEcho: true}
}

// rms16 returns the RMS amplitude of little-endian 16-bit PCM.
func rms16(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(int16(pcm[2*i]) | int16(pcm[2*i+1])<<8)
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}

// keepInbound decides whether an inbound 8 kHz PCM chunk should be fed to the
// pipeline. When the bot is not speaking, everything passes. While the bot is
// speaking, only audio clearly louder than the tracked echo floor passes (the
// caller barging in); the quiet, steady echo is dropped and used to update the
// floor. This keeps barge-in working without the bot hearing itself.
func (s *Serializer) keepInbound(pcm8 []byte) bool {
	rms := rms16(pcm8)
	s.mu.Lock()
	defer s.mu.Unlock()
	botIdle := time.Now().After(s.playbackEndsAt.Add(echoTail))
	if rms > bargeAbsFloor { // actual speech: remember it for latency timing
		s.lastSpeechAt = time.Now()
		// Only start a latency measurement on a clean turn (bot not already
		// speaking). Otherwise the bot's own echo or a barge-in mid-reply
		// would start a bogus timer that the next outbound frame closes at
		// ~0 ms, poisoning the median.
		if botIdle {
			s.awaitingBot = true
		}
	}
	if botIdle {
		s.echoFloor = 0 // bot silent: pass everything
		return true
	}
	if s.echoFloor == 0 {
		s.echoFloor = rms
	}
	if rms > bargeAbsFloor && rms > s.echoFloor*bargeFactor {
		return true // clearly louder than the echo: real barge-in
	}
	s.echoFloor = (1-echoFloorEMA)*s.echoFloor + echoFloorEMA*rms
	return false
}

// noteOutbound advances the estimated playback-end clock by the duration of a
// chunk of outbound audio (given as sample count at telnyxRate) and, on the
// first reply audio of a turn, reports the response latency.
func (s *Serializer) noteOutbound(samples int) {
	dur := time.Duration(samples) * time.Second / telnyxRate
	s.mu.Lock()
	now := time.Now()
	if now.After(s.playbackEndsAt) {
		s.playbackEndsAt = now
	}
	s.playbackEndsAt = s.playbackEndsAt.Add(dur)
	var latency time.Duration
	if s.awaitingBot && !s.lastSpeechAt.IsZero() {
		latency = now.Sub(s.lastSpeechAt)
		s.awaitingBot = false
	}
	hook := s.onLatency
	s.mu.Unlock()
	if latency > 0 && hook != nil {
		hook(latency)
	}
}

// resetPlayback marks playback as finished (used on interruption/clear, when
// Telnyx flushes its buffer).
func (s *Serializer) resetPlayback() {
	s.mu.Lock()
	s.playbackEndsAt = time.Now()
	s.mu.Unlock()
}

// BotActive reports whether the bot is estimated to still be playing audio.
func (s *Serializer) BotActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.playbackEndsAt.Add(echoTail))
}

// PlaybackEnd returns the estimated time the bot's audio finishes playing.
func (s *Serializer) PlaybackEnd() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.playbackEndsAt
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
	mulaw, err := base64.StdEncoding.DecodeString(m.Media.Payload)
	if err != nil {
		return nil, nil
	}
	pcm8 := MuLawToPCM16(mulaw)
	if s.suppressEcho && !s.keepInbound(pcm8) {
		return nil, nil // steady echo while the bot speaks: drop it
	}
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
