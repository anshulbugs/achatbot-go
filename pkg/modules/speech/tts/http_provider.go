package tts

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/weedge/pipeline-go/pkg/logger"

	"achatbot/pkg/common"
)

// VoiceProvider is a TTS provider whose voice and speed can be set per session.
// Both the local sherpa-onnx provider and the HTTP (GPU service) provider
// implement it, so the pipeline can use either behind one type.
type VoiceProvider interface {
	common.ITTSProvider
	SetVoice(sid int, speed float32)
}

// kokoroSidToName maps our numeric speaker ids to the Kokoro package voice
// names used by the GPU TTS service. Ids match server.go's kokoroVoices.
var kokoroSidToName = map[int]string{
	0: "af_alloy", 1: "af_aoede", 2: "af_bella", 3: "af_heart", 4: "af_jessica",
	5: "af_kore", 6: "af_nicole", 7: "af_nova", 8: "af_river", 9: "af_sarah", 10: "af_sky",
	11: "am_adam", 12: "am_echo", 13: "am_eric", 14: "am_fenrir", 15: "am_liam",
	16: "am_michael", 17: "am_onyx", 18: "am_puck", 19: "am_santa",
	20: "bf_alice", 21: "bf_emma", 22: "bf_isabella", 23: "bf_lily",
	24: "bm_daniel", 25: "bm_fable", 26: "bm_george", 27: "bm_lewis",
	45: "zf_xiaobei", 46: "zf_xiaoni", 47: "zf_xiaoxiao", 48: "zf_xiaoyi",
	49: "zm_yunjian", 50: "zm_yunxi", 51: "zm_yunxia", 52: "zm_yunyang",
}

func voiceNameFor(sid int) string {
	if n, ok := kokoroSidToName[sid]; ok {
		return n
	}
	return "af_heart"
}

// HTTPTTSProvider synthesizes speech via a remote GPU Kokoro service that
// returns raw little-endian PCM16 mono at 24 kHz. Instances are cheap (just an
// HTTP client), so the pool can be large; the GPU service handles concurrency.
type HTTPTTSProvider struct {
	baseURL string
	sid     int
	speed   float32
	client  *http.Client
	name    string
}

const httpTTSRate = 24000

// NewHTTPTTSProvider builds a provider pointing at baseURL (e.g.
// http://127.0.0.1:8880). Returns nil if the service is unreachable at startup.
func NewHTTPTTSProvider(baseURL string, sid int, speed float32) *HTTPTTSProvider {
	p := &HTTPTTSProvider{
		baseURL: baseURL,
		sid:     sid,
		speed:   speed,
		client:  &http.Client{Timeout: 30 * time.Second},
		name:    "kokoroHTTP",
	}
	resp, err := p.client.Get(baseURL + "/health")
	if err != nil {
		logger.Error("HTTP TTS health check failed", "url", baseURL, "err", err)
		return nil
	}
	resp.Body.Close()
	return p
}

// SetVoice changes the speaker/speed for subsequent syntheses.
func (p *HTTPTTSProvider) SetVoice(sid int, speed float32) {
	if sid >= 0 {
		p.sid = sid
	}
	if speed > 0 {
		p.speed = speed
	}
}

func (p *HTTPTTSProvider) request(text string) (*http.Response, error) {
	body, _ := json.Marshal(map[string]any{
		"input": text,
		"voice": voiceNameFor(p.sid),
		"speed": p.speed,
	})
	return p.client.Post(p.baseURL+"/tts", "application/json", bytes.NewReader(body))
}

// Synthesize returns the whole utterance as PCM16.
func (p *HTTPTTSProvider) Synthesize(text string) []byte {
	resp, err := p.request(text)
	if err != nil {
		logger.Error("HTTP TTS request failed", "err", err)
		return nil
	}
	defer resp.Body.Close()
	pcm, _ := io.ReadAll(resp.Body)
	return pcm
}

// SynthesizeStream streams the PCM back in ~100 ms chunks as it arrives, on
// even (2-byte) boundaries so no sample is split. onAudio returns false to stop.
func (p *HTTPTTSProvider) SynthesizeStream(text string, onAudio func(pcm []byte) bool) {
	resp, err := p.request(text)
	if err != nil {
		logger.Error("HTTP TTS stream request failed", "err", err)
		return
	}
	defer resp.Body.Close()

	buf := make([]byte, httpTTSRate*2*100/1000) // 100 ms
	var carry byte
	haveCarry := false
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if haveCarry {
				chunk = append([]byte{carry}, chunk...)
				haveCarry = false
			}
			if len(chunk)%2 == 1 {
				carry = chunk[len(chunk)-1]
				haveCarry = true
				chunk = chunk[:len(chunk)-1]
			}
			if len(chunk) > 0 && !onAudio(chunk) {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}

func (p *HTTPTTSProvider) Warmup() {}

// GetSampleInfo returns the fixed Kokoro output format: 24 kHz mono 16-bit.
func (p *HTTPTTSProvider) GetSampleInfo() (int, int, int) { return httpTTSRate, 1, 2 }

func (p *HTTPTTSProvider) SetPromptAudio(string, []byte) error { return nil }
func (p *HTTPTTSProvider) Name() string                        { return p.name }
func (p *HTTPTTSProvider) Reset() error                        { return nil }
func (p *HTTPTTSProvider) Release() error                      { return nil }

var _ VoiceProvider = (*HTTPTTSProvider)(nil)
var _ VoiceProvider = (*SherpaOnnxProvider)(nil)
