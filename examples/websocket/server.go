package main

import (
	"cmp"
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/weedge/pipeline-go/pkg/frames"
	"github.com/weedge/pipeline-go/pkg/logger"
	"github.com/weedge/pipeline-go/pkg/pipeline"
	"github.com/weedge/pipeline-go/pkg/processors"
	"github.com/weedge/pipeline-go/pkg/serializers"

	"achatbot/pkg/common"
	"achatbot/pkg/config"
	"achatbot/pkg/consts"
	"achatbot/pkg/modules/functions"
	"achatbot/pkg/modules/llm"
	"achatbot/pkg/modules/speech/asr"
	"achatbot/pkg/modules/speech/tts"
	"achatbot/pkg/modules/speech/vad_analyzer"
	"achatbot/pkg/params"
	achatbot_processors "achatbot/pkg/processors"
	achatbot_aggregators "achatbot/pkg/processors/aggregators"
	"achatbot/pkg/processors/llm_processors"
	"achatbot/pkg/services/middleware"
	"achatbot/pkg/telnyx"
	"achatbot/pkg/transports"
	"achatbot/pkg/turngate"
	"achatbot/pkg/types"
	achatbot_frames "achatbot/pkg/types/frames"
)

//go:embed ui/index.html ui/protobuf.min.js ui/data_frames.proto
var uiFS embed.FS

// Upgrader for upgrading HTTP connections to WebSocket connections
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// Allow connections from any origin in this example
		// In production, you should be more restrictive
		return true
	},
}

// Global variables to manage server state
var (
	serverMu    sync.Mutex
	activeTasks = make(map[*pipeline.PipelineTask]bool)

	cfg                       *config.Config
	vadPool, asrPool, ttsPool *common.ModuleProviderPool
)

// userTranscriptName tags TextFrames carrying the user's own transcribed
// speech, so the browser distinguishes them from bot replies (whose frames
// are named "TextFrame"). The serializer emits it as "<name>#<n>".
const userTranscriptName = "user_transcript"

// kokoroVoice describes one selectable speaker of the kokoro multi-lang model.
// IDs follow the model's alphabetical voice ordering (zh voices 45-52 match
// the constants in pkg/modules/speech/tts).
type kokoroVoice struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Lang string `json:"lang"` // "en" or "zh"
}

// premiumVoiceOrder are the voices kokoro itself grades highest in VOICES.md,
// best first: af_heart (A), af_bella (A-), bf_emma (B-). Every other English
// voice is graded C+ or below and the gap is audible, so these are served ahead
// of the rest and grouped separately in the UI. Order is meaningful — the list
// is emitted in this sequence, so the top entry is the strongest voice.
var premiumVoiceOrder = []int{3, 2, 21} // Heart, Bella, Emma

func isPremiumVoice(id int) bool {
	for _, p := range premiumVoiceOrder {
		if p == id {
			return true
		}
	}
	return false
}

// apiVoice is kokoroVoice as served to the UI, tagged with the premium flag.
// Kept separate so the voice table itself stays a plain positional literal.
type apiVoice struct {
	kokoroVoice
	Premium bool `json:"premium,omitempty"`
}

var kokoroVoices = []kokoroVoice{
	{0, "Alloy (US female)", "en"}, {1, "Aoede (US female)", "en"}, {2, "Bella (US female)", "en"},
	{3, "Heart (US female)", "en"}, {4, "Jessica (US female)", "en"}, {5, "Kore (US female)", "en"},
	{6, "Nicole (US female)", "en"}, {7, "Nova (US female)", "en"}, {8, "River (US female)", "en"},
	{9, "Sarah (US female)", "en"}, {10, "Sky (US female)", "en"},
	{11, "Adam (US male)", "en"}, {12, "Echo (US male)", "en"}, {13, "Eric (US male)", "en"},
	{14, "Fenrir (US male)", "en"}, {15, "Liam (US male)", "en"}, {16, "Michael (US male)", "en"},
	{17, "Onyx (US male)", "en"}, {18, "Puck (US male)", "en"}, {19, "Santa (US male)", "en"},
	{20, "Alice (UK female)", "en"}, {21, "Emma (UK female)", "en"}, {22, "Isabella (UK female)", "en"},
	{23, "Lily (UK female)", "en"},
	{24, "Daniel (UK male)", "en"}, {25, "Fable (UK male)", "en"}, {26, "George (UK male)", "en"},
	{27, "Lewis (UK male)", "en"},
	{45, "Xiaobei (zh female)", "zh"}, {46, "Xiaoni (zh female)", "zh"}, {47, "Xiaoxiao (zh female)", "zh"},
	{48, "Xiaoyi (zh female)", "zh"}, {49, "Yunjian (zh male)", "zh"}, {50, "Yunxi (zh male)", "zh"},
	{51, "Yunxia (zh male)", "zh"}, {52, "Yunyang (zh male)", "zh"},
}

// languageNames maps the UI language codes to the human name used in the LLM
// instruction and to the SenseVoice language hint.
var languageNames = map[string]string{
	"en": "English", "zh": "Chinese", "ja": "Japanese", "ko": "Korean", "yue": "Cantonese",
}

func isValidVoiceID(id int) bool {
	for _, v := range kokoroVoices {
		if v.ID == id {
			return true
		}
	}
	return false
}

// wavHeader builds a 44-byte PCM WAV header for 16-bit mono audio.
func wavHeader(dataLen, sampleRate int) []byte {
	h := make([]byte, 44)
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], uint32(36+dataLen))
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16)
	binary.LittleEndian.PutUint16(h[20:22], 1)
	binary.LittleEndian.PutUint16(h[22:24], 1)
	binary.LittleEndian.PutUint32(h[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(h[28:32], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(h[32:34], 2)
	binary.LittleEndian.PutUint16(h[34:36], 16)
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], uint32(dataLen))
	return h
}

func writeCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

// ollamaModels lists model names from the Ollama native API, derived from the
// configured OpenAI-compatible base URL. Returns just the configured model
// when the endpoint is not Ollama or unreachable.
func ollamaModels() []string {
	fallback := []string{cfg.LLM.Model}
	base := strings.TrimSuffix(cfg.LLM.BaseURL, "/v1")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(base + "/api/tags")
	if err != nil {
		return fallback
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&tags) != nil || len(tags.Models) == 0 {
		return fallback
	}
	names := make([]string, 0, len(tags.Models))
	for _, m := range tags.Models {
		names = append(names, m.Name)
	}
	return names
}

// handleOptions serves the selectable settings for the UI.
func handleOptions(w http.ResponseWriter, r *http.Request) {
	writeCORS(w)
	if r.Method == http.MethodOptions {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// Premium voices first, in graded order, then everything else as listed.
	voices := make([]apiVoice, 0, len(kokoroVoices))
	for _, id := range premiumVoiceOrder {
		for _, v := range kokoroVoices {
			if v.ID == id {
				voices = append(voices, apiVoice{kokoroVoice: v, Premium: true})
			}
		}
	}
	for _, v := range kokoroVoices {
		if !isPremiumVoice(v.ID) {
			voices = append(voices, apiVoice{kokoroVoice: v})
		}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"voices":     voices,
		"llm_models": ollamaModels(),
		"telephony":  telnyxClient != nil,
		"current": map[string]any{
			"llm_model":  cfg.LLM.Model,
			"asr_model":  cfg.ASR.Model,
			"tts_engine": cfg.TTS.Model,
			"vad_model":  cfg.VAD.Model,
			"voice":      cfg.TTS.SpeakerID,
			"speed":      cfg.TTS.Speed,
			// The UI posts these back per call, so it must start from what the
			// server is configured with. Shipping only the models meant the
			// prompt box kept a hardcoded copy that silently overrode
			// config.yaml on every phone call.
			"system_prompt": cfg.Server.SystemPrompt,
			"hello":         cfg.Server.InboundHello,
			"avatar_url":    cfg.Server.AvatarURL,
		},
	})
}

// handleTTSPreview synthesizes a short sample with the requested voice/speed
// and returns it as WAV, so the UI can preview voices before a call.
func handleTTSPreview(w http.ResponseWriter, r *http.Request) {
	writeCORS(w)
	if r.Method == http.MethodOptions {
		return
	}
	voiceID, err := strconv.Atoi(r.URL.Query().Get("voice"))
	if err != nil || !isValidVoiceID(voiceID) {
		http.Error(w, "unknown voice id", http.StatusBadRequest)
		return
	}
	speed := 1.0
	if s := r.URL.Query().Get("speed"); s != "" {
		if speed, err = strconv.ParseFloat(s, 32); err != nil || speed <= 0.2 || speed > 3 {
			http.Error(w, "speed must be in (0.2, 3]", http.StatusBadRequest)
			return
		}
	}
	text := r.URL.Query().Get("text")
	if text == "" {
		text = "Hi there! This is how I sound. Shall we talk?"
	}
	if len(text) > 200 {
		text = text[:200]
	}

	info, err := ttsPool.Get()
	if err != nil {
		http.Error(w, "all voices are busy right now, try again in a moment", http.StatusServiceUnavailable)
		return
	}
	defer ttsPool.Put(info)
	provider := info.GetInstance().(tts.VoiceProvider)
	provider.SetVoice(voiceID, float32(speed))
	pcm := provider.Synthesize(text)
	rate, _, _ := provider.GetSampleInfo()

	w.Header().Set("Content-Type", "audio/wav")
	w.Write(wavHeader(len(pcm), rate))
	w.Write(pcm)
}

// ExampleIWebSocketConn wraps *websocket.Conn to implement our IWebSocketConn interface
type ExampleIWebSocketConn struct {
	*websocket.Conn
	mu sync.Mutex
}

// ReadMessage implements the IWebSocketConn interface
func (wsc *ExampleIWebSocketConn) ReadMessage() (messageType consts.MessageType, p []byte, err error) {
	var msType int
	msType, p, err = wsc.Conn.ReadMessage()
	return consts.MessageType(msType), p, err
}

// WriteMessage implements the IWebSocketConn interface
func (wsc *ExampleIWebSocketConn) WriteMessage(messageType consts.MessageType, data []byte) error {
	if (len(data)) < 200 { //for text frame
		println("WriteMessage-->", messageType.String(), len(data), string(data))
	} else {
		println("WriteMessage-->", messageType.String(), len(data))
	}
	// NOTE: don't concurrent write to websocket connection, need lock
	// issue: concurrent write TextFrame and AudioRawFrame
	wsc.mu.Lock()
	err := wsc.Conn.WriteMessage(int(messageType), data)
	wsc.mu.Unlock()

	return err

}

// Close implements the IWebSocketConn interface
func (wsc *ExampleIWebSocketConn) Close() error {
	println("Close websocket connection")
	return wsc.Conn.Close()
}

func load(cfg *config.Config) (*common.ModuleProviderPool, *common.ModuleProviderPool, *common.ModuleProviderPool) {
	var err error
	// vad
	vadPoolType := reflect.TypeOf(&vad_analyzer.SherpaOnnxProvider{})
	common.RegisterNewFunc(vadPoolType, func() (common.IPoolInstance, error) {
		vadConfig := vad_analyzer.NewDefaultSherpaOnnxVadModelConfig(cfg.VAD.Model)
		vadConfig.NumThreads = cfg.VAD.NumThreads
		sherpaOnnxProvider := vad_analyzer.NewSherpaOnnxProvider(
			vadConfig,
			cfg.VAD.BufferSizeSeconds,
		)
		if sherpaOnnxProvider == nil {
			return nil, fmt.Errorf("failed to create VAD provider for model %q (model file downloaded?)", cfg.VAD.Model)
		}
		return sherpaOnnxProvider, nil
	})
	vadPool := common.NewModuleProviderPool(cfg.VAD.PoolSize, vadPoolType)
	err = vadPool.Initialize()
	if err != nil {
		log.Fatal(err)
	}

	// asr
	var asrPoolType reflect.Type
	if cfg.ASR.Model == "parakeet_http" {
		// GPU ASR service (Parakeet on NeMo) over HTTP.
		asrPoolType = reflect.TypeOf(&asr.HTTPASRProvider{})
		common.RegisterNewFunc(asrPoolType, func() (common.IPoolInstance, error) {
			p := asr.NewHTTPASRProvider(cfg.ASR.HTTPURL)
			if p == nil {
				return nil, fmt.Errorf("failed to create HTTP ASR provider at %s (service up?)", cfg.ASR.HTTPURL)
			}
			return p, nil
		})
	} else {
		asrConfig, err := asr.NewOfflineRecognizerConfigForModel(cfg.ASR.Model)
		if err != nil {
			log.Fatal(err)
		}
		asrConfig.ModelConfig.NumThreads = cfg.ASR.NumThreads
		asrConfig = asr.WithLanguage(asrConfig, cfg.ASR.Language)
		asrPoolType = reflect.TypeOf(&asr.SherpaOnnxProvider{})
		common.RegisterNewFunc(asrPoolType, func() (common.IPoolInstance, error) {
			sherpaOnnxProvider := asr.NewSherpaOnnxProvider(asrConfig)
			if sherpaOnnxProvider == nil {
				return nil, fmt.Errorf("failed to create ASR provider for model %q (model files downloaded?)", cfg.ASR.Model)
			}
			return sherpaOnnxProvider, nil
		})
	}
	asrPool := common.NewModuleProviderPool(cfg.ASR.PoolSize, asrPoolType)
	err = asrPool.Initialize()
	if err != nil {
		log.Fatal(err)
	}

	// tts: local sherpa (CPU) or an HTTP GPU service (kokoro_http / voxtral_http).
	var ttsPoolType reflect.Type
	if cfg.TTS.Model == "voxtral_http" {
		ttsPoolType = reflect.TypeOf(&tts.HTTPTTSProvider{})
		common.RegisterNewFunc(ttsPoolType, func() (common.IPoolInstance, error) {
			p := tts.NewOpenAISpeechProvider(cfg.TTS.HTTPURL, "mistralai/Voxtral-4B-TTS-2603", cfg.TTS.HTTPVoice, cfg.TTS.Speed, cfg.TTS.Gain)
			if p == nil {
				return nil, fmt.Errorf("failed to reach Voxtral TTS service at %s", cfg.TTS.HTTPURL)
			}
			return p, nil
		})
	} else if cfg.TTS.Model == "dots_http" {
		ttsPoolType = reflect.TypeOf(&tts.HTTPTTSProvider{})
		common.RegisterNewFunc(ttsPoolType, func() (common.IPoolInstance, error) {
			p := tts.NewContractProvider(cfg.TTS.HTTPURL, cfg.TTS.HTTPVoice, "dotsHTTP", cfg.TTS.Speed, cfg.TTS.Gain, 48000)
			if p == nil {
				return nil, fmt.Errorf("failed to reach dots.tts service at %s", cfg.TTS.HTTPURL)
			}
			return p, nil
		})
	} else if cfg.TTS.Model == "kani_http" {
		ttsPoolType = reflect.TypeOf(&tts.HTTPTTSProvider{})
		common.RegisterNewFunc(ttsPoolType, func() (common.IPoolInstance, error) {
			p := tts.NewKaniProvider(cfg.TTS.HTTPURL, cfg.TTS.HTTPVoice, cfg.TTS.Speed, cfg.TTS.Gain)
			if p == nil {
				return nil, fmt.Errorf("failed to reach Kani TTS service at %s", cfg.TTS.HTTPURL)
			}
			return p, nil
		})
	} else if cfg.TTS.Model == "maya_http" {
		// Maya1: expressive TTS. Speaks the same /tts contract as kokoro, so
		// it needs no provider of its own — only a different URL and rate.
		// cfg.TTS.HTTPVoice selects a persona (warm/excited/apologetic/firm),
		// which is where the emotional style lives; inline tags in the text
		// carry vocal events. See deploy/tts/maya_server.py.
		ttsPoolType = reflect.TypeOf(&tts.HTTPTTSProvider{})
		common.RegisterNewFunc(ttsPoolType, func() (common.IPoolInstance, error) {
			p := tts.NewContractProvider(cfg.TTS.HTTPURL, cfg.TTS.HTTPVoice, "mayaHTTP", cfg.TTS.Speed, cfg.TTS.Gain, 24000)
			if p == nil {
				return nil, fmt.Errorf("failed to reach Maya1 TTS service at %s", cfg.TTS.HTTPURL)
			}
			return p, nil
		})
	} else if cfg.TTS.Model == "kokoro_http" {
		ttsPoolType = reflect.TypeOf(&tts.HTTPTTSProvider{})
		common.RegisterNewFunc(ttsPoolType, func() (common.IPoolInstance, error) {
			p := tts.NewHTTPTTSProvider(cfg.TTS.HTTPURL, cfg.TTS.SpeakerID, cfg.TTS.Speed, cfg.TTS.Gain)
			if p == nil {
				return nil, fmt.Errorf("failed to reach TTS service at %s", cfg.TTS.HTTPURL)
			}
			return p, nil
		})
	} else {
		ttsPoolType = reflect.TypeOf(&tts.SherpaOnnxProvider{})
		common.RegisterNewFunc(ttsPoolType, func() (common.IPoolInstance, error) {
			ttsConfig := tts.NewDefaultSherpaOnnxOfflineTtsConfig()
			ttsConfig.Model.NumThreads = cfg.TTS.NumThreads
			sherpaOnnxProvider := tts.NewSherpaOnnxProvider(ttsConfig, cfg.TTS.SpeakerID, cfg.TTS.Speed, cfg.TTS.Model+"TTS")
			if sherpaOnnxProvider == nil {
				return nil, fmt.Errorf("failed to create TTS provider for model %q (model files downloaded?)", cfg.TTS.Model)
			}
			return sherpaOnnxProvider, nil
		})
	}
	ttsPool := common.NewModuleProviderPool(cfg.TTS.PoolSize, ttsPoolType)
	err = ttsPool.Initialize()
	if err != nil {
		log.Fatal(err)
	}

	return vadPool, asrPool, ttsPool
}

// newLLMProcessor builds the configured LLM provider and wraps it in the
// matching pipeline processor. openai_api works against any OpenAI-compatible
// endpoint (OpenAI, OpenRouter, Ollama /v1, vLLM); ollama_api uses the native
// Ollama client (endpoint from OLLAMA_HOST).
// newLLMProcessor builds the LLM provider and processor. model overrides the
// configured model when non-empty (per-session selection from the UI).
func newLLMProcessor(cfg *config.Config, session *common.Session, model string) (processors.IFrameProcessor, error) {
	if model == "" {
		model = cfg.LLM.Model
	}
	switch cfg.LLM.Provider {
	case "ollama_api":
		var thinking *string
		if cfg.LLM.Thinking != "" {
			thinking = &cfg.LLM.Thinking
		}
		provider := llm.NewOllamaAPIProvider(llm.OllamaAPIProviderName, model, cfg.LLM.Stream, thinking, nil, cfg.LLM.Tools)
		if provider == nil {
			return nil, fmt.Errorf("failed to create ollama_api LLM provider for model %q", model)
		}
		return llm_processors.NewLLMOllamaApiProcessor(provider, session, llm_processors.Mode_Chat), nil
	case "openai_api":
		provider := llm.NewOpenAIAPIProvider(llm.OpenAIAPIProviderName, cfg.LLM.BaseURL, model, cfg.LLM.Tools)
		if provider == nil {
			return nil, fmt.Errorf("failed to create openai_api LLM provider for model %q at %s", model, cfg.LLM.BaseURL)
		}
		// Advertise this session's own tools alongside the globally configured
		// ones. Registering an implementation without advertising it is a
		// silent no-op — the model never learns the tool exists, so it never
		// calls it, and the failure looks like "the model refuses to transfer"
		// rather than "the tool was never offered".
		provider.AddTools(session.ToolCalls())
		return llm_processors.NewLLMOpenAIApiProcessor(provider, session, llm_processors.Mode_Chat, cfg.LLM.Stream, *llmArgs()), nil
	default:
		return nil, fmt.Errorf("unknown llm.provider %q", cfg.LLM.Provider)
	}
}

// llmArgs builds the sampling arguments from config. The library defaults are
// reasonable except for max tokens: 2048 is far longer than anyone listens to
// on a call, and a long generation holds a decode slot for its whole length,
// so it costs concurrency as well as patience.
func llmArgs() *types.LMGenerateArgs {
	a := types.NewLMGenerateArgs()
	if cfg.LLM.Temperature > 0 {
		a.LmGenTemperature = cfg.LLM.Temperature
	}
	if cfg.LLM.MaxTokens > 0 {
		a.LmGenMaxTokens = cfg.LLM.MaxTokens
	}
	return a
}

// sessionOverrides holds per-connection settings parsed from ws query params.
type sessionOverrides struct {
	voiceID      int
	speed        float32
	volume       float32
	llmModel     string
	lang         string // "" | en | zh | ja | ko | yue
	hello        string // greeting text, synthesized directly to speech on connect
	prompt       string // full system prompt override (breaks prefix sharing)
	promptSuffix string // per-caller text appended to the shared base
}

func parseSessionOverrides(r *http.Request) sessionOverrides {
	o := sessionOverrides{voiceID: -1, speed: 0}
	q := r.URL.Query()
	if v, err := strconv.Atoi(q.Get("voice")); err == nil && isValidVoiceID(v) {
		o.voiceID = v
	}
	if s, err := strconv.ParseFloat(q.Get("speed"), 32); err == nil && s > 0.2 && s <= 3 {
		o.speed = float32(s)
	}
	if g, err := strconv.ParseFloat(q.Get("volume"), 32); err == nil && g > 0.2 && g <= 3 {
		o.volume = float32(g)
	}
	if m := q.Get("llm"); m != "" && len(m) <= 100 {
		o.llmModel = m
	}
	if l := q.Get("lang"); languageNames[l] != "" {
		o.lang = l
	}
	if h := q.Get("hello"); h != "" {
		if len(h) <= maxHelloBytes {
			o.hello = h
		} else {
			log.Printf("WARNING: greeting override ignored — %d bytes exceeds the %d-byte limit; using the server default",
				len(h), maxHelloBytes)
		}
	}
	if p := q.Get("prompt"); p != "" {
		if len(p) <= maxPromptBytes {
			o.prompt = p
		} else {
			log.Printf("WARNING: system-prompt override ignored — %d bytes exceeds the %d-byte limit; using the server default",
				len(p), maxPromptBytes)
		}
	}
	if s := q.Get("prompt_suffix"); s != "" {
		if len(s) <= maxPromptBytes {
			o.promptSuffix = s
		} else {
			log.Printf("WARNING: prompt suffix ignored — %d bytes exceeds the %d-byte limit",
				len(s), maxPromptBytes)
		}
	}
	return o
}

// Caps on client-supplied overrides, in BYTES (len on a Go string counts
// bytes, and an em dash or smart quote costs three).
//
// The old prompt cap was 4000, which was smaller than the server's OWN
// configured prompt. The UI round-trips that prompt through the textarea, so
// every browser call sent ~12.5 KB, tripped the cap, and silently fell back to
// the server default — making the prompt box look broken with nothing logged
// anywhere. Any cap here must stay comfortably above whatever a deployment
// puts in config.yaml.
const (
	maxPromptBytes = 32 * 1024
	maxHelloBytes  = 2000
)

// resolvePrompt builds the system prompt for a session, preferring a suffix
// appended to the shared base over a wholesale replacement.
//
// This ordering is a throughput decision, not a style one. SGLang's RadixAttention
// caches shared *prefixes*, so when every caller starts from the same base the
// ~2.9k-token prompt is prefilled once for the whole fleet. Put per-caller text in
// front of it and that sharing disappears: measured at 60 concurrent requests,
// tenant-text-first ran 10.8 req/s while tenant-text-last ran 31.5 req/s on the
// same two GPUs. Same model, same hardware, purely where the custom text sits.
func resolvePrompt(base, replace, suffix string) string {
	if replace != "" {
		// Caller supplied a whole prompt: honoured, but it shares no prefix with
		// anyone else, so it costs roughly 3x the LLM capacity of a suffix.
		if suffix != "" {
			return replace + "\n\n" + suffix
		}
		return replace
	}
	if suffix != "" {
		return base + "\n\n" + suffix
	}
	return base
}

// callStyleRules are appended to every system prompt, on every call.
//
// These are TTS rules, not personality. The model is writing something a
// speech engine will read aloud, and the failure modes are specific and
// consistent enough to be worth stating on every call rather than hoping each
// tenant's prompt remembers them:
//
//   - Written filler ("hm", "uh") is not the natural hesitation it looks like
//     on the page. TTS pronounces it as a word, which lands as a glitch rather
//     than as thinking.
//   - Digit strings read as quantities are unusable on a call. A phone number
//     spoken as "three hundred twenty one million..." cannot be written down,
//     and the caller has no way to ask for it again except to ask for all of it.
//
// Appended LAST, after the tenant's own prompt. Later instructions carry more
// weight with the model, and these have to survive a 3000-token prompt that
// never mentions them. The cost is that they sit after the per-contact block
// and are re-prefilled per call — about eighty tokens, which is nothing next to
// what a cold campaign prefix costs.
const callStyleRules = `
Delivery rules for this call, which override any conflicting instruction above:
- You are speaking on a live phone call. Everything you write is read aloud by a speech engine, so write words to be spoken, never text to be read. No markdown, no bullet points, no emoji, no symbols, no stage directions.
- Do not write filler sounds or written laughter. Never write "hm", "hmm", "uh", "um", "er", "ah", "aha", "ha", "haha", "heh" or similar. A speech engine pronounces them as words, so they land as a fault rather than as thinking or amusement. If you need a pause, use a comma or a short sentence; if something is funny, say so in words.
- Use the person's name sparingly. Once when you greet them is plenty, and perhaps once more at the very end. Never open or close consecutive replies with it. On a call, hearing your own name after every sentence sounds like a script, not a conversation.
- Say every number one digit at a time, grouped for the ear. "3214528106" is "three two one, four five two, eight one zero six". Do the same for phone numbers, reference numbers, codes and account numbers.`

// withCallStyle appends the delivery rules, and the greeting already spoken, to
// a system prompt.
//
// ORDER MATTERS TWICE OVER. The tenant's prompt comes first and the per-contact
// greeting last, so everything two calls of one campaign have in common sits at
// the front where the LLM's prefix cache can share it — the same reason §9 of
// the contract asks the platform to put contact details last.
//
// And the greeting goes in the PROMPT rather than into chat history as an
// assistant turn. History is a record of the conversation; the greeting was
// spoken by a speech engine the model never saw. Stating it plainly in the
// instructions is both more honest and easier for the model to act on — with it
// there the model answers the caller instead of opening with a preamble of its
// own.
func withCallStyle(prompt, spokenGreeting string) string {
	out := prompt
	if out == "" {
		out = strings.TrimSpace(callStyleRules)
	} else {
		out += "\n" + callStyleRules
	}
	if spokenGreeting != "" {
		out += "\n\n## What has already happened on this call\n" +
			"You have ALREADY said this opening line, out loud, and the person has just " +
			"replied to it:\n\n\"" + spokenGreeting + "\"\n\n" +
			"The call is under way. Do not say any of it again, do not reintroduce " +
			"yourself, and do not open with a fresh greeting. Answer what they just said " +
			"and carry the conversation forward. If they only said \"hello\", acknowledge " +
			"it in a few words and put your first real question to them."
	}
	return out
}

// firstChars returns up to n runes of s on a single line, for log lines that
// need to identify a prompt without dumping the whole thing. Rune-based so a
// cut never lands mid-character and corrupts the log.
func firstChars(s string, n int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", "")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// systemPromptFor returns the base system prompt with an optional
// "respond only in <language>" instruction appended for the chosen language.
func systemPromptFor(lang string) string {
	prompt := cfg.Server.SystemPrompt
	if name := languageNames[lang]; name != "" {
		prompt += " Always respond only in " + name + "."
	}
	return prompt
}

// sessionConfig fully describes one voice session, independent of transport.
type sessionConfig struct {
	clientID     string
	systemPrompt string
	voiceID      int
	speed        float32
	volume       float32
	llmModel     string
	addWavHeader bool   // true for the browser; false for raw telephony audio
	hello        string // optional greeting synthesized and played on connect
	// spokenGreeting is the greeting the caller has ALREADY heard, played
	// before the pipeline started. Distinct from hello, which asks this
	// function to play one.
	spokenGreeting     string
	allowInterruptions bool            // browser: barge-in; telephony: false (half-duplex, echo-safe)
	idlePrompt         string          // spoken after idleSecs of silence ("" disables)
	idleSecs           float64         // silence threshold before idlePrompt fires
	audioOutFrameMS    int             // outbound audio framing; small = lower first-audio latency
	demoVoices         []int           // if set, play a sample in each voice then converse in voiceID
	stop               <-chan struct{} // when closed, cancels the pipeline task (used by the load test)
	// chatObserver receives every chat turn as it is appended, for the
	// platform's end-of-call transcript. nil on the demo path.
	chatObserver func(map[string]any)
	// agentTurnObserver receives each agent turn as it is handed to speech,
	// truncated at an interruption.
	agentTurnObserver func(string)
	// callID is the carrier call-control id, used to move this call's capacity
	// state to on_gpu. Empty for browser sessions, which hold pool slots but
	// are not part of the platform's call accounting.
	callID string
	// call is the telephony call this session belongs to, used to bind
	// per-call tools such as call_transfer. nil for browser sessions.
	call *callParams
}

// voiceLabel returns the spoken name of a voice id (e.g. "Bella"), stripping the
// "(US female)" qualifier used in the UI list.
func voiceLabel(id int) string {
	for _, v := range kokoroVoices {
		if v.ID == id {
			if i := strings.Index(v.Name, " ("); i > 0 {
				return v.Name[:i]
			}
			return v.Name
		}
	}
	return "this"
}

// handleWebSocket serves the browser studio client over protobuf/PCM16.
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	overrides := parseSessionOverrides(r)
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Error upgrading to WebSocket: %v", err)
		return
	}
	defer conn.Close()

	prompt := resolvePrompt(systemPromptFor(overrides.lang), overrides.prompt, overrides.promptSuffix)
	runVoiceSession(&ExampleIWebSocketConn{Conn: conn}, serializers.NewProtobufSerializer(), sessionConfig{
		clientID:     fmt.Sprintf("%s_%s", conn.RemoteAddr().Network(), conn.RemoteAddr().String()),
		systemPrompt: prompt,
		voiceID:      overrides.voiceID,
		speed:        overrides.speed,
		volume:       overrides.volume,
		llmModel:     overrides.llmModel,
		// Fall back to the configured greeting when the client sent none.
		//
		// The demo page omits the greeting whenever it matches the server's own
		// — sending an identical copy achieves nothing and puts it in a URL
		// where proxies have opinions about length. That is the right call, but
		// it only works if the server actually falls back, and this path did
		// not: the browser simply got silence where the greeting should be,
		// while inbound phone calls (which read the same config value directly)
		// were fine. The symptom is "my configured greeting is not spoken",
		// with nothing in the logs to suggest why.
		hello:              cmp.Or(overrides.hello, cfg.Server.InboundHello),
		addWavHeader:       true,
		allowInterruptions: cfg.Server.AllowInterruptions,
	})
}

// sendAudioChunks synthesizes-then-streams pre-rendered PCM to the client as
// ~20 ms AudioRawFrames via the serializer, used to play the greeting before
// the conversation loop starts.
// ttsSampleInfo reports the TTS service's output format without holding a pool
// slot for the duration -- announcements need the rate to pace playback.
func ttsSampleInfo() (int, int, int) {
	info, err := ttsPool.Get()
	if err != nil {
		return consts.DefaultRate, 1, 2
	}
	defer ttsPool.Put(info)
	return info.GetInstance().(tts.VoiceProvider).GetSampleInfo()
}

func sendAudioChunks(tw *achatbot_processors.WebsocketTransportWriter, pcm []byte, rate int) {
	chunk := rate * 2 * 100 / 1000 // 100 ms chunks limit per-chunk resample boundary artifacts
	if chunk <= 0 {
		return
	}
	for off := 0; off < len(pcm); off += chunk {
		end := off + chunk
		if end > len(pcm) {
			end = len(pcm)
		}
		// SendAudioFrame, not SendPayload: the browser needs a WAV header on
		// every chunk or decodeAudioData rejects it and the audio is silently
		// dropped. This path carries the greeting, the voice demo and the idle
		// re-prompt — none of which go through the pipeline's WriteRawAudio.
		if err := tw.SendAudioFrame(frames.NewAudioRawFrame(pcm[off:end], rate, 1, 2)); err != nil {
			return
		}
	}
}

// hangupConn wraps a websocket connection and closes done on the first read
// error, which is how a hangup reaches us. runVoiceSession uses that signal to
// cancel the pipeline task so the session's pool instances are released.
type hangupConn struct {
	common.IWebSocketConn
	once sync.Once
	done chan struct{}
}

func (c *hangupConn) ReadMessage() (consts.MessageType, []byte, error) {
	mt, p, err := c.IWebSocketConn.ReadMessage()
	if err != nil {
		c.once.Do(func() { close(c.done) })
	}
	return mt, p, err
}

func (c *hangupConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.IWebSocketConn.Close()
}

// runVoiceSession builds and runs the VAD->ASR->LLM->TTS pipeline over the
// given connection and serializer. The browser and telephony transports differ
// only in the serializer, WAV framing, and greeting; everything else is shared.
func runVoiceSession(wsConn common.IWebSocketConn, serializer serializers.Serializer, sc sessionConfig) {
	// A real client hanging up only surfaces as a read error on the socket. Without
	// this, task.Run() below blocks forever, the pool-release defers never fire, and
	// every finished call permanently leaks its VAD/ASR/TTS slots.
	if sc.stop == nil {
		hung := &hangupConn{IWebSocketConn: wsConn, done: make(chan struct{})}
		wsConn, sc.stop = hung, hung.done
	}

	chatHistorySize := cfg.Server.ChatHistorySize
	// Promote this call from `reserved` to `on_gpu`: the pipeline is about to
	// hold a VAD, an ASR and a TTS slot for its whole life. Release is driven
	// by the carrier's hangup rather than by this function returning, since a
	// call ends for the platform when the CALL ends, not when our media
	// session does. Empty clientID (browser sessions) is not tracked.
	if sc.callID != "" {
		markOnGPU(sc.callID)
	}

	session := common.NewSession(sc.clientID, &chatHistorySize)
	// Capture every turn for the end-of-call transcript. This has to observe
	// appends rather than read the buffer afterwards: chatHistorySize bounds
	// the buffer to a rolling window, so by the end of a long call the earlier
	// turns are gone. nil on the demo path, which keeps no transcript.
	if sc.chatObserver != nil {
		session.GetChatHistory().SetObserver(sc.chatObserver)
	}
	// The agent's own side of the conversation, recorded as it is spoken rather
	// than as it is generated. See Session.SetAgentTurnObserver.
	if sc.agentTurnObserver != nil {
		session.SetAgentTurnObserver(sc.agentTurnObserver)
		// Feed back how much audio actually reached the caller, so a turn cut
		// short is reported as what they heard rather than what was generated.
		if h, ok := serializer.(interface{ SetSpokenHook(func(time.Duration)) }); ok {
			h.SetSpokenHook(session.RecordSpokenAudio)
		}
		// Characters per second for this voice and speed. Kokoro runs at about
		// 14 at normal pace; speed scales it near enough for cutting a sentence
		// at a word boundary.
		session.SetSpeakingRate(14 * float64(sc.speed))
	}
	// Only when a greeting was already spoken: the caller is answering the
	// phone, not the agent, so their opening "hello" carries nothing to reply
	// to. Without a greeting there is nothing for them to be acknowledging and
	// their first word is a real turn.
	session.SetFilterPickupNoise(sc.spokenGreeting != "")
	// Report per-turn LLM latency. The first turn of each call drives
	// backpressure on its own — see pkg/rexa/metrics.go.
	sessionID := sc.clientID
	session.SetLLMObserver(func(ttft time.Duration, turn int) {
		observeLLMTurn(sessionID, ttft, turn)
	})
	// Bind per-call tools. Registered on the session rather than globally
	// because they act on THIS call, and a global handler could not tell which
	// of the calls in flight invoked it.
	if sc.call != nil && sc.callID != "" {
		registerTransferTool(session, sc.call, sc.callID)
		// Repair the case where the model TELLS the caller it is connecting
		// them and never invokes the tool. Installed after the observer above
		// so it runs alongside whatever else watches agent turns.
		session.SetAgentTurnObserver(chainAgentObservers(
			sc.agentTurnObserver, transferOnPromise(session, sc.callID)))
	}
	session.InitChatMessage(map[string]any{
		"role": "system", "content": withCallStyle(sc.systemPrompt, sc.spokenGreeting),
	})

	// Log which prompt this session actually got. Answering "did my prompt
	// apply?" previously meant reading code and guessing, because an override
	// that was rejected looked identical to one that was never sent.
	log.Printf("session %s: system prompt %d bytes, starts %q",
		sc.clientID, len(sc.systemPrompt), firstChars(sc.systemPrompt, 80))

	// Track caller activity for the idle re-prompt.
	var actMu sync.Mutex
	lastUser := time.Now()
	idlePrompts := 0
	touchUser := func() {
		actMu.Lock()
		lastUser = time.Now()
		idlePrompts = 0
		actMu.Unlock()
	}

	// vad provider
	vadPoolInstanceInfo, err := vadPool.Get()
	if err != nil {
		log.Printf("Get VAD instance from pool err: %v", err)
		return
	}
	defer vadPool.Put(vadPoolInstanceInfo)
	vadProvider := vadPoolInstanceInfo.GetInstance().(*vad_analyzer.SherpaOnnxProvider)
	vadArgs := params.NewVADAnalyzerArgs().WithStartSecs(cfg.VAD.StartSecs).WithStopSecs(cfg.VAD.StopSecs)
	vadAnalyzer := vad_analyzer.NewVADAnalyzer(vadArgs, vadProvider)

	// Create audio VAD parameters
	audioCameraParams := params.NewAudioCameraParams()
	audioCameraParams.AudioVADParams.WithVADAnalyzer(vadAnalyzer).
		WithVADEnabled(true).WithVADAudioPassthrough(true)
	audioCameraParams.AudioVADParams.AudioParams.
		WithAudioInEnabled(true).WithAudioOutEnabled(true).
		WithAudioInSampleRate(consts.DefaultRate).WithAudioInSampleWidth(consts.DefaultSampleWidth).WithAudioInChannels(consts.DefaultChannels)

	// Create WebSocket server parameters
	wsParams := &params.WebsocketServerParams{
		AudioCameraParams: audioCameraParams,
		Serializer:        serializer,
	}
	frameMS := sc.audioOutFrameMS
	if frameMS <= 0 {
		frameMS = 200
	}
	wsParams.WithAudioOutFrameMS(frameMS).WithAudioOutAddWavHeader(sc.addWavHeader)

	// Set Websocket Transport Writer
	transportWriter := achatbot_processors.NewWebsocketTransportWriter(wsConn, wsParams)
	audioCameraParams.WithTransportWriter(transportWriter).WithAudioOutEnabled(true).
		WithAudioOutSampleWidth(consts.DefaultSampleWidth).WithAudioOutSampleRate(consts.DefaultRate).WithAudioOutChannels(consts.DefaultChannels)

	// Set ASR Processor
	asrPoolInstanceInfo, err := asrPool.Get()
	if err != nil {
		log.Printf("Get ASR instance from pool err: %v", err)
		return
	}
	defer asrPool.Put(asrPoolInstanceInfo)
	asrProvider := asrPoolInstanceInfo.GetInstance().(common.IASRProvider)
	asrProcessor := achatbot_processors.NewASRProcessor(asrProvider).
		WithOnTranscript(func(text string) {
			touchUser() // the caller spoke: reset the idle timer
			// Surface the user's transcript to the client, tagged so the UI
			// can render it as the user's turn (bot text uses "TextFrame").
			// Harmless for telephony: the Telnyx serializer drops TextFrames.
			frame := &frames.TextFrame{
				DataFrame: frames.NewDataFrameWithName(userTranscriptName),
				Text:      text,
			}
			if err := transportWriter.SendPayload(frame); err != nil {
				log.Printf("send transcript err: %v", err)
			}
		})

	// Set TTS Processor
	ttsPoolInstanceInfo, err := ttsPool.Get()
	if err != nil {
		log.Printf("Get tts instance from pool err: %v", err)
		return
	}
	defer ttsPool.Put(ttsPoolInstanceInfo)
	ttsProvider := ttsPoolInstanceInfo.GetInstance().(tts.VoiceProvider)
	// Pool instances keep the previous session's voice: reset to config
	// defaults first, then apply overrides (no-ops when unset).
	ttsProvider.SetVoice(cfg.TTS.SpeakerID, cfg.TTS.Speed)
	ttsProvider.SetVoice(sc.voiceID, sc.speed)
	ttsProvider.SetGain(cfg.TTS.Gain) // reset to config default, then per-session
	if sc.volume > 0 {
		ttsProvider.SetGain(sc.volume)
	}
	ttsProcessor := achatbot_processors.NewTTSProcessor(ttsProvider)
	outRate, outChannels, outSampleWidth := ttsProvider.GetSampleInfo()
	audioCameraParams.WithAudioOutSampleWidth(outSampleWidth).WithAudioOutSampleRate(outRate).WithAudioOutChannels(outChannels)

	// Play the greeting (if any) before the conversation loop. Safe to
	// synthesize directly here: the pipeline's TTS processor is not yet active.
	if len(sc.demoVoices) > 0 {
		// Voice-demo call: speak the same sample in each candidate voice,
		// paced to roughly real time so they play in order, then leave the
		// configured voiceID active for any conversation that follows.
		go func() {
			for _, vid := range sc.demoVoices {
				ttsProvider.SetVoice(vid, sc.speed)
				text := "This is the " + voiceLabel(vid) + " voice. I can help you book appointments and answer questions on a call."
				pcm := ttsProvider.Synthesize(text)
				sendAudioChunks(transportWriter, pcm, outRate)
				dur := time.Duration(len(pcm)/2/outRate)*time.Second + 800*time.Millisecond
				time.Sleep(dur)
			}
			ttsProvider.SetVoice(sc.voiceID, sc.speed)
		}()
	} else if sc.hello != "" {
		go sendAudioChunks(transportWriter, ttsProvider.Synthesize(sc.hello), outRate)
	}

	// Idle re-prompt (telephony): if the line goes silent for idleSecs with no
	// bot or caller audio, speak idlePrompt ("Are you still there?") up to
	// twice before giving up. Pre-synthesize once to avoid racing the pipeline
	// TTS instance.
	if sc.idlePrompt != "" && sc.idleSecs > 0 {
		if tser, ok := serializer.(*telnyx.Serializer); ok {
			idlePCM := ttsProvider.Synthesize(sc.idlePrompt)
			idleDur := time.Duration(sc.idleSecs * float64(time.Second))
			idleStop := make(chan struct{})
			defer close(idleStop)
			go func() {
				t := time.NewTicker(time.Second)
				defer t.Stop()
				var lastPrompt time.Time
				for {
					select {
					case <-idleStop:
						return
					case <-t.C:
						if tser.BotActive() {
							continue
						}
						actMu.Lock()
						ref := lastUser
						np := idlePrompts
						actMu.Unlock()
						if pe := tser.PlaybackEnd(); pe.After(ref) {
							ref = pe
						}
						// Live speech counts as activity, not just finished
						// transcripts: a caller mid-sentence has no transcript yet
						// and would otherwise be interrupted by the re-prompt.
						if ls := tser.LastSpeechAt(); ls.After(ref) {
							ref = ls
						}
						if time.Since(ref) >= idleDur && np < 2 && time.Since(lastPrompt) >= idleDur {
							sendAudioChunks(transportWriter, idlePCM, outRate)
							lastPrompt = time.Now()
							actMu.Lock()
							idlePrompts++
							actMu.Unlock()
						}
					}
				}
			}()
		}
	}

	// Set LLM Processor from config plus per-session overrides
	llmProcessor, err := newLLMProcessor(cfg, session, sc.llmModel)
	if err != nil {
		log.Printf("Create LLM processor err: %v", err)
		return
	}
	log.Printf("session %s: voice=%d speed=%.2f llm=%q", sc.clientID, sc.voiceID, sc.speed, sc.llmModel)

	// Set Sentence Processor: flush the opening ~4 words of each reply fast so
	// TTS (and the caller) start immediately, then normal sentence boundaries.
	sentenceProcessor := achatbot_aggregators.NewFastFirstAggregatorWithEnd(
		reflect.TypeOf(&achatbot_frames.TurnEndFrame{}), cfg.Server.FirstChunkWords)

	// 1. Create the WebSocket server input processor
	ws_transport := transports.NewWebsocketTransport(
		wsConn,
		wsParams,
	)

	// 2. Assemble the pipeline. The optional turn gate (semantic endpointing)
	// sits between ASR and the LLM: it holds partial utterances until the
	// caller has finished, so vad.stop_secs can stay short.
	procs := []processors.IFrameProcessor{
		processors.NewDefaultFrameLoggerProcessorWithIncludeFrame(
			[]frames.Frame{&frames.StartFrame{}, &frames.EndFrame{}, &frames.CancelFrame{}},
		),
		processors.NewDefaultFrameLoggerProcessorWithIncludeFrame([]frames.Frame{&achatbot_frames.BotSpeakingFrame{}}).WithMaxIdToLogs([]uint64{}),

		ws_transport.InputProcessor(),
		achatbot_aggregators.NewAudioResponseAggregatorWithAccumulate(
			reflect.TypeOf(&achatbot_frames.UserStartedSpeakingFrame{}),
			reflect.TypeOf(&achatbot_frames.UserStoppedSpeakingFrame{}),
			reflect.TypeOf(&achatbot_frames.VADStateAudioRawFrame{}),
		),
		processors.NewDefaultFrameLoggerProcessorWithIncludeFrame([]frames.Frame{&frames.AudioRawFrame{}, &achatbot_frames.VADStateAudioRawFrame{}}),
		asrProcessor.WithPassRawAudio(false),
		processors.NewDefaultFrameLoggerProcessorWithIncludeFrame([]frames.Frame{&frames.TextFrame{}}),
	}
	if cfg.Server.TurnGateEnabled {
		gate := turngate.New(cfg.LLM.BaseURL, cfg.Server.TurnGateModel)
		gateProc := achatbot_processors.NewTurnGateProcessor(
			gate, time.Duration(cfg.Server.TurnGateMaxWaitSecs*float64(time.Second)),
		).WithSession(session).WithOnDecide(func(refined string, complete bool) {
			logger.Infof("turn gate: complete=%v refined=%q", complete, refined)
		})
		procs = append(procs, gateProc)
	}
	procs = append(procs,
		llmProcessor,
		sentenceProcessor,
		processors.NewDefaultFrameLoggerProcessorWithIncludeFrame([]frames.Frame{&frames.TextFrame{}}),
		ttsProcessor.WithPassText(true),
		processors.NewDefaultFrameLoggerProcessorWithIncludeFrame([]frames.Frame{&frames.AudioRawFrame{}}),
		processors.NewDefaultFrameLoggerProcessorWithIncludeFrame([]frames.Frame{&frames.AudioRawFrame{}}),
		ws_transport.OutputProcessor(),
	)
	myPipeline := pipeline.NewPipelineWithVerbose(procs, nil, nil, false)
	logger.Info(myPipeline.String())

	// In a real application, you would integrate this with your frame processing pipeline
	// and properly manage the processor lifecycle
	// 3. Create and run a pipeline task
	// NOTE: set IsPushBlock: false, IsUpPushBlock: false to debug queue frame and check slow process
	task := pipeline.NewPipelineTask(myPipeline, pipeline.PipelineParams{
		AllowInterruptions: sc.allowInterruptions,
		IsPushBlock:        true,
		IsUpPushBlock:      true,
	})

	// Add task to active tasks map
	serverMu.Lock()
	activeTasks[task] = true
	serverMu.Unlock()

	// Remove task from active tasks when done; pool instances are released by
	// the defers registered right after each Get, covering early returns too.
	defer func() {
		serverMu.Lock()
		delete(activeTasks, task)
		serverMu.Unlock()
	}()

	// External stop (load test): cancel the task so task.Run() returns and the
	// pool-release defers fire, instead of blocking forever on a closed conn.
	if sc.stop != nil {
		go func() {
			<-sc.stop
			task.Cancel()
		}()
	}

	task.Run()
}

func main() {
	configPath := flag.String("config", "", "path to config.yaml (default: discover config.yaml in . or ./configs, else built-in defaults)")
	flag.Parse()

	var err error
	cfg, err = config.Load(*configPath)
	if err != nil {
		log.Fatalf("Load config failed: %v", err)
	}
	// Fail fast on unknown tool names; otherwise every connection would fail
	// at LLM-provider creation instead.
	for _, name := range cfg.LLM.Tools {
		if functions.RegisterFuncs.Get(name) == nil {
			log.Fatalf("llm.tools: function %q is not registered", name)
		}
	}

	logger.InitLoggerWithConfig(logger.NewDefaultLoggerConfig())
	logger.Info("Loaded config",
		"vad.model", cfg.VAD.Model, "vad.pool_size", cfg.VAD.PoolSize,
		"asr.model", cfg.ASR.Model, "asr.pool_size", cfg.ASR.PoolSize,
		"tts.model", cfg.TTS.Model, "tts.speaker_id", cfg.TTS.SpeakerID, "tts.pool_size", cfg.TTS.PoolSize,
		"llm.provider", cfg.LLM.Provider, "llm.model", cfg.LLM.Model, "llm.base_url", cfg.LLM.BaseURL,
	)
	// Must precede load(): the speech providers capture their HTTP transport
	// when their clients are constructed, so wrapping it afterwards would
	// silently record nothing.
	initRexaTelemetry()
	vadPool, asrPool, ttsPool = load(cfg)

	// Create HTTP server
	server := &http.Server{
		Addr: cfg.Server.Addr,
	}

	// Set up the WebSocket endpoint with Rate Limiter middleware
	rateLimiter := middleware.NewDefaultRateLimiter().WithEnable(cfg.Server.RateLimitEnabled).WithMaxConns(cfg.Server.MaxConns)
	http.HandleFunc("/api/options", handleOptions)
	http.HandleFunc("/api/tts-preview", handleTTSPreview)
	http.HandleFunc("/api/loadtest", handleLoadTest)

	// Telephony (optional): enabled when TELNYX_API_KEY is set.
	telnyxClient = telnyx.NewClientFromEnv()
	if telnyxClient != nil {
		http.HandleFunc("/api/call", handleCall)
		logger.Info("Telephony enabled", "from", telnyxClient.FromNumber(), "public_url", telnyxClient.PublicURL())
	} else {
		logger.Info("Telephony disabled (TELNYX_API_KEY not set)")
	}

	// Platform call-agent contract (optional): enabled when both REXA_*
	// secrets are set. Registered after telnyxClient so it can fall back to
	// that client's public URL.
	rexaEnabled := registerRexaRoutes(http.DefaultServeMux)

	// The carrier webhook + media bridge serve BOTH paths, so they are
	// registered when either is active — the contract supplies its telecom
	// credentials per dispatch, so it needs these even with TELNYX_API_KEY
	// unset. Registering a pattern twice on a mux panics, hence the single
	// combined condition rather than one block per path.
	if telnyxClient != nil || rexaEnabled {
		http.HandleFunc("/telnyx/webhook", handleTelnyxWebhook)
		http.HandleFunc("/telnyx/media", handleTelnyxMedia)
	}

	// The Daily sidecar's media bridge. Registered unconditionally: it is
	// harmless without a dispatch behind it (404s) and the contract endpoints
	// are wired separately.
	http.HandleFunc("/room/media", handleRoomMedia)
	// The live-listen relay: caller audio out to an operator's room, one way.
	// Same wire as /room/media, opposite meaning — there the room is the
	// caller, here it is somebody listening to one.
	http.HandleFunc("/relay/media", handleRelayMedia)

	// Browser voice WS moves to /ws so the UI can be served at /.
	http.Handle("/ws", rateLimiter.Middleware(http.HandlerFunc(handleWebSocket)))

	// Serve the embedded demo UI (index.html, protobuf.min.js, data_frames.proto)
	// at the root, so the public tunnel URL serves the whole app.
	uiSub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		log.Fatalf("embed ui: %v", err)
	}
	http.Handle("/", http.FileServer(http.FS(uiSub)))

	// Channel to listen for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Run server in a goroutine
	go func() {
		logger.Info("Starting WebSocket server on " + cfg.Server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed to start: %v", err)
		}
	}()

	// Wait for interrupt signal
	<-sigChan
	logger.Info("Shutdown signal received")

	// Create a context with timeout for graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Shutdown the HTTP server gracefully
	logger.Info("Shutting down HTTP server...")
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	// Cancel all active pipeline tasks
	logger.Info("Cancelling all active pipeline tasks...")
	serverMu.Lock()
	for task := range activeTasks {
		// Send a cancel frame to the pipeline
		task.Cancel()
	}
	serverMu.Unlock()

	// close pool
	vadPool.Close()
	asrPool.Close()
	ttsPool.Close()

	// Wait a bit for tasks to finish cleanup
	time.Sleep(1 * time.Second)

	logger.Info("Server exited gracefully")
}
