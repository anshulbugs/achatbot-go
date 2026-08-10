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

// echoTail is how long after the bot's audio finishes playing we keep the echo
// gate active, to catch the tail of acoustic echo. playbackEndsAt is only an
// estimate, and a phone line's reverb outlives the audio that caused it: at
// 100ms the bot's own tail was reaching ASR and coming back as short spurious
// transcripts ("Hello." answered with a greeting, which then echoed again).
// 250ms covers the tail without eating the start of a real barge-in, which has
// to clear bargeAbsFloor anyway.
const echoTail = 250 * time.Millisecond

// preRoll is how much recently-gated inbound audio is replayed into the
// pipeline when a barge-in is detected. The echo gate can only recognise speech
// once it is loud enough to clear the echo floor, but a word ramps up over
// ~50-150ms, so by then its onset has already been dropped and ASR receives a
// truncated utterance — "Whereabouts do you live?" arrived as "When way do you
// leave?". Replaying the audio immediately before the trigger restores the
// missing onset. It may carry a little echo with it, which ASR tolerates far
// better than a missing first syllable.
const preRoll = 240 * time.Millisecond

// preRollBytes is preRoll as a byte count of 8 kHz 16-bit mono PCM.
const preRollBytes = int(preRoll/time.Millisecond) * telnyxRate / 1000 * 2

// gateHold is how long the echo gate stays fully open after speech, once the
// caller has barged in. It must exceed the pauses inside a normal sentence, so
// it tracks vad.stop_secs rather than being tuned separately.
const gateHold = 800 * time.Millisecond

// interruptMute is how long outbound audio is dropped after an interruption, to
// swallow the interrupted turn's tail still draining through the pipeline.
// Measured reply latency on this path is ~950ms at best, so a new turn's audio
// lands well after this window and is never clipped.
const interruptMute = 400 * time.Millisecond

// Adaptive echo-gate tuning. While the bot is speaking, inbound audio is
// mostly its own echo at a low, stable level; the caller interrupting is
// clearly louder. We track the echo floor and let audio through only when it
// exceeds both an absolute floor and a multiple of that echo level.
const (
	// Lowered from 550 with the same intent as bargeFactor below: the agent was
	// still talking a beat after the caller started. A phone handset delivers
	// ordinary speech well above this, and the echo it has to reject is bounded
	// separately by bargeFactor, so this floor exists to ignore line noise
	// rather than to judge loudness.
	bargeAbsFloor = 420.0 // min inbound RMS (int16) to count as speech at all
	// Lowered from 2.0 after a live call: the agent kept talking for a
	// noticeable moment after the caller started. Echo returns attenuated by
	// the handset, so 1.6 still clears it, and the pre-roll buffer below means
	// a slightly earlier trigger costs nothing — the words that opened the
	// barge-in are replayed to ASR either way.
	bargeFactor  = 1.6  // inbound must exceed echoFloor * this to be barge-in
	echoFloorEMA = 0.15 // smoothing for the running echo-floor estimate
)

// biquad is a Direct-Form-I second-order IIR filter used to clean outbound
// telephone audio. A high-pass removes sub-300 Hz rumble that only muddies the
// narrow µ-law band, and a presence peak lifts the 2-3 kHz consonant energy that
// carries intelligibility over a phone line.
type biquad struct {
	b0, b1, b2, a1, a2 float64
	x1, x2, y1, y2     float64
}

func (f *biquad) process(x float64) float64 {
	y := f.b0*x + f.b1*f.x1 + f.b2*f.x2 - f.a1*f.y1 - f.a2*f.y2
	f.x2, f.x1 = f.x1, x
	f.y2, f.y1 = f.y1, y
	return y
}

// newHighpass / newPeaking build RBJ-cookbook biquads normalized by a0.
func newHighpass(fs, f0, q float64) *biquad {
	w0 := 2 * math.Pi * f0 / fs
	c, s := math.Cos(w0), math.Sin(w0)
	alpha := s / (2 * q)
	a0 := 1 + alpha
	return &biquad{
		b0: (1 + c) / 2 / a0, b1: -(1 + c) / a0, b2: (1 + c) / 2 / a0,
		a1: -2 * c / a0, a2: (1 - alpha) / a0,
	}
}

func newPeaking(fs, f0, q, dBgain float64) *biquad {
	A := math.Pow(10, dBgain/40)
	w0 := 2 * math.Pi * f0 / fs
	c, s := math.Cos(w0), math.Sin(w0)
	alpha := s / (2 * q)
	a0 := 1 + alpha/A
	return &biquad{
		b0: (1 + alpha*A) / a0, b1: -2 * c / a0, b2: (1 - alpha*A) / a0,
		a1: -2 * c / a0, a2: (1 - alpha/A) / a0,
	}
}

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
	echoFloor      float64   // running estimate of inbound echo RMS during bot speech
	preRollBuf     []byte    // recently gated inbound audio, replayed on barge-in
	gateOpenUntil  time.Time // while set, the caller is mid-sentence: pass everything

	// Wire-level response-latency measurement: time from the caller's last
	// speech frame to the bot's first reply audio.
	lastSpeechAt time.Time
	awaitingBot  bool
	onLatency    func(time.Duration)
	// onSpoken reports each chunk of audio actually sent to the caller, which
	// is the only measure of what they heard. Text is generated far faster than
	// it is spoken, so an interrupted turn's transcript can only be trimmed
	// honestly with this.
	onSpoken func(time.Duration)

	// Post-interruption fence. Sending Telnyx a "clear" flushes only what it has
	// already buffered; it does nothing about TTS audio for the interrupted turn
	// that is still draining through the pipeline behind us. Cancellation is
	// asynchronous, so those frames keep arriving here for a while and Telnyx
	// happily plays them — the caller interrupts and then hears the tail of the
	// reply they just cut off.
	//
	// The fence is a deadline, never a flag. Gating it on a "new turn has begun"
	// frame is what a first attempt did, and it silenced the call completely: no
	// such frame is ever emitted in this pipeline, so the mute latched on the
	// first interruption and never lifted. A deadline degrades safely — worst
	// case a little tail leaks through, which is the bug we started with rather
	// than a dead call.
	mutedUntil time.Time
	// tap receives a copy of inbound caller audio, for live listening. nil on
	// every call nobody is listening to, which is almost all of them.
	tap func(pcm8 []byte, rate int)
	// deaf stops inbound audio reaching the pipeline. See Deafen.
	deaf bool

	// holdInterruptsUntil suppresses the "clear" event while audio we have
	// already sent is still playing at Telnyx.
	//
	// The greeting is handed to Telnyx in full and plays from its buffer, so
	// when our side considers it "finished" the carrier is still playing it.
	// The pipeline starts at that moment and emits an early interruption frame,
	// which becomes a "clear", which flushes the unplayed remainder — the
	// caller hears the greeting stop mid-sentence at the same point every time.
	holdInterruptsUntil time.Time

	// Outbound clarity filtering (telephone voice enhancement).
	clarity  bool
	hpf      *biquad
	presence *biquad
}

// SupportsInterruption reports that this serializer encodes an interruption on
// the wire (as Telnyx's "clear" event), so callers should hand it the frame
// rather than fall back to a client-side control message.
func (s *Serializer) SupportsInterruption() bool { return true }

// AllowInterrupts drops the greeting hold, so the next interruption frame
// really does flush the carrier's buffer.
//
// The hold exists to stop the pipeline flushing a greeting Telnyx is still
// playing. Answering-machine detection wants the opposite — cut the greeting
// NOW and leave a message — so it needs a way past. Without it the voicemail
// message queues behind the whole greeting and plays long after the beep.
func (s *Serializer) AllowInterrupts() {
	s.mu.Lock()
	s.holdInterruptsUntil = time.Time{}
	s.mu.Unlock()
}

// HoldInterrupts suppresses "clear" events for d, covering audio already sent
// but not yet played.
func (s *Serializer) HoldInterrupts(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	s.holdInterruptsUntil = time.Now().Add(d)
	s.mu.Unlock()
}

// SetClarity enables/disables the outbound clarity filter.
func (s *Serializer) SetClarity(on bool) { s.clarity = on }

// applyClarity high-pass filters and presence-boosts the 8 kHz PCM in place.
func (s *Serializer) applyClarity(pcm8 []byte) {
	if !s.clarity || s.hpf == nil {
		return
	}
	for i := 0; i+1 < len(pcm8); i += 2 {
		v := float64(int16(uint16(pcm8[i]) | uint16(pcm8[i+1])<<8))
		v = s.presence.process(s.hpf.process(v))
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		iv := int16(v)
		pcm8[i] = byte(iv)
		pcm8[i+1] = byte(uint16(iv) >> 8)
	}
}

// SetLatencyHook registers a callback fired once per turn with the measured
// time from the caller's last speech to the bot's first reply audio.
func (s *Serializer) SetLatencyHook(fn func(time.Duration)) { s.onLatency = fn }

// SetSpokenHook registers a callback fired with the duration of every chunk of
// outbound audio.
func (s *Serializer) SetSpokenHook(fn func(time.Duration)) { s.onSpoken = fn }

// NewSerializer builds a Telnyx media serializer for the given pipeline sample
// rate (typically 16000), with half-duplex echo suppression enabled.
func NewSerializer(pipelineRate int) *Serializer {
	if pipelineRate == 0 {
		pipelineRate = consts.DefaultRate
	}
	return &Serializer{
		pipelineRate: pipelineRate,
		suppressEcho: true,
		clarity:      true,
		hpf:          newHighpass(telnyxRate, 200, 0.707),  // cut sub-200 Hz rumble
		presence:     newPeaking(telnyxRate, 2600, 0.9, 4), // +4 dB presence at 2.6 kHz
	}
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

// flushPreRoll returns pcm8 prefixed by any gated audio held back, and empties
// the buffer. Callers must hold s.mu.
func (s *Serializer) flushPreRoll(pcm8 []byte) []byte {
	if len(s.preRollBuf) == 0 {
		return pcm8
	}
	out := make([]byte, 0, len(s.preRollBuf)+len(pcm8))
	out = append(out, s.preRollBuf...)
	out = append(out, pcm8...)
	s.preRollBuf = s.preRollBuf[:0]
	return out
}

// keepInbound decides whether an inbound 8 kHz PCM chunk should be fed to the
// pipeline. When the bot is not speaking, everything passes. While the bot is
// speaking, only audio clearly louder than the tracked echo floor passes (the
// caller barging in); the quiet, steady echo is dropped and used to update the
// floor. This keeps barge-in working without the bot hearing itself.
// It returns the audio to feed the pipeline: nil when the chunk is gated, and
// otherwise the chunk itself, prefixed by any gated audio held in the pre-roll
// buffer so a barge-in keeps its word onset.
func (s *Serializer) keepInbound(pcm8 []byte) []byte {
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
		s.preRollBuf = s.preRollBuf[:0]
		s.gateOpenUntil = time.Time{}
		return pcm8
	}
	if s.echoFloor == 0 {
		s.echoFloor = rms
	}
	// Once the caller has barged in, the gate stays open for the rest of their
	// sentence. Filtering frame by frame instead punches holes through the
	// middle of it: ordinary speech dips below the echo floor between words and
	// on unvoiced consonants, and those frames were being discarded, so ASR
	// received the sentence with pieces missing. Keeping it open costs nothing,
	// because the barge-in has already told the bot to stop talking.
	now := time.Now()
	if now.Before(s.gateOpenUntil) {
		if rms > bargeAbsFloor {
			s.gateOpenUntil = now.Add(gateHold) // still talking: hold it open
		}
		return s.flushPreRoll(pcm8)
	}
	if rms > bargeAbsFloor && rms > s.echoFloor*bargeFactor {
		s.gateOpenUntil = now.Add(gateHold)
		return s.flushPreRoll(pcm8)
	}
	s.echoFloor = (1-echoFloorEMA)*s.echoFloor + echoFloorEMA*rms
	s.preRollBuf = append(s.preRollBuf, pcm8...)
	if n := len(s.preRollBuf) - preRollBytes; n > 0 {
		s.preRollBuf = append(s.preRollBuf[:0], s.preRollBuf[n:]...)
	}
	return nil
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
	spokenHook := s.onSpoken
	s.mu.Unlock()
	if spokenHook != nil {
		spokenHook(dur)
	}
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

// SetInboundTap installs a callback receiving a COPY of the caller's audio, at
// 8 kHz, before the echo gate. nil removes it.
//
// For live listening: it lets an operator's room be fed from the call without
// putting a second reader on the media socket, which is the thing that cannot
// be done safely while the pipeline owns it. The copy is deliberate — a tap
// that handed out the pipeline's own buffer could change what the agent hears.
func (s *Serializer) SetInboundTap(fn func(pcm8 []byte, rate int)) {
	s.mu.Lock()
	s.tap = fn
	s.mu.Unlock()
}

func (s *Serializer) inboundTap() func([]byte, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tap
}

// Hush stops this call's outbound audio for good.
//
// FOR THE HANDOVER GAP. When an operator barges, the agent cannot simply be
// dropped: the SIP leg carrying the operator takes about five seconds to be
// answered — Daily only registers a room's SIP endpoint once a session exists —
// and cutting the agent first leaves the caller with nobody at all.
//
// So it stays on the line and stops speaking. The caller hears the agent go
// quiet the moment the operator commits, instead of hearing it talk over the
// handover, and once conferenced it can no longer hear itself and stutter.
// Deliberately permanent: this call is being handed to a person, and there is
// no state in which the agent should start talking again.
func (s *Serializer) Hush() {
	s.mu.Lock()
	s.mutedUntil = time.Now().Add(24 * time.Hour)
	s.mu.Unlock()
}

// Deafen stops feeding the pipeline, while the tap keeps running.
//
// MUTING THE OUTPUT IS NOT ENOUGH. Hush only drops audio at the last step: VAD,
// ASR, the LLM and TTS all carry on, on the GPU, producing replies that are
// then thrown away. On a barged call that is a whole pipeline's compute spent
// on a conversation the agent is no longer part of — and barged calls are
// exactly the long ones, because a person is on them.
//
// Starving the input is what actually stops the work. With no frames the VAD
// never fires, so nothing downstream runs. The tap is upstream of this, so the
// operator still hears the caller.
//
// The call keeps its pool instances and so still counts against the GPU
// ceiling. That is deliberate: releasing the slot while holding VAD/ASR/TTS
// instances would let the next dispatch in against capacity that does not
// exist.
func (s *Serializer) Deafen() {
	s.mu.Lock()
	s.deaf = true
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

// LastSpeechAt returns when inbound audio last reached speech level. The idle
// re-prompt needs this: a transcript only lands once the caller pauses, so a
// caller mid-sentence looks idle if you only watch transcripts, and the bot
// talks over them asking "are you still there?".
func (s *Serializer) LastSpeechAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSpeechAt
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

	// Tap the caller's audio BEFORE the echo gate.
	//
	// An operator listening in wants to hear the call as it is, not as the
	// pipeline needs it: the gate exists to stop the agent transcribing itself,
	// and applying it here would cut the caller out of the operator's ear
	// whenever the agent happened to be speaking. Taking a copy this early also
	// means the tap cannot change what the pipeline receives.
	if tap := s.inboundTap(); tap != nil {
		cp := make([]byte, len(pcm8))
		copy(cp, pcm8)
		tap(cp, telnyxRate)
	}

	// Handed to an operator: the tap above still feeds their room, but nothing
	// downstream of here should run. See Deafen.
	s.mu.Lock()
	deaf := s.deaf
	s.mu.Unlock()
	if deaf {
		return nil, nil
	}

	if s.suppressEcho {
		if pcm8 = s.keepInbound(pcm8); pcm8 == nil {
			return nil, nil // steady echo while the bot speaks: drop it
		}
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
		s.mu.Lock()
		holding := time.Now().Before(s.holdInterruptsUntil)
		s.mu.Unlock()
		if holding {
			// Flushing now would cut the greeting Telnyx is still playing.
			return nil, nil
		}
		s.resetPlayback()
		s.mu.Lock()
		s.mutedUntil = time.Now().Add(interruptMute)
		s.mu.Unlock()
		return json.Marshal(map[string]string{"event": "clear"})
	case *frames.AudioRawFrame:
		if len(af.Audio) == 0 {
			return nil, nil
		}
		s.mu.Lock()
		muted := time.Now().Before(s.mutedUntil)
		s.mu.Unlock()
		if muted {
			return nil, nil // tail of the turn the caller just interrupted
		}
		pcm8 := ResamplePCM16(af.Audio, af.SampleRate, telnyxRate)
		s.applyClarity(pcm8) // high-pass + presence boost for phone clarity
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
