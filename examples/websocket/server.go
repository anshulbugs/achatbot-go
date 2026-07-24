package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
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
	json.NewEncoder(w).Encode(map[string]any{
		"voices":     kokoroVoices,
		"llm_models": ollamaModels(),
		"telephony":  telnyxClient != nil,
		"current": map[string]any{
			"llm_model":  cfg.LLM.Model,
			"asr_model":  cfg.ASR.Model,
			"tts_engine": cfg.TTS.Model,
			"vad_model":  cfg.VAD.Model,
			"voice":      cfg.TTS.SpeakerID,
			"speed":      cfg.TTS.Speed,
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
	provider := info.GetInstance().(*tts.SherpaOnnxProvider)
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
	asrConfig, err := asr.NewOfflineRecognizerConfigForModel(cfg.ASR.Model)
	if err != nil {
		log.Fatal(err)
	}
	asrConfig.ModelConfig.NumThreads = cfg.ASR.NumThreads
	asrConfig = asr.WithLanguage(asrConfig, cfg.ASR.Language)
	asrPoolType := reflect.TypeOf(&asr.SherpaOnnxProvider{})
	common.RegisterNewFunc(asrPoolType, func() (common.IPoolInstance, error) {
		sherpaOnnxProvider := asr.NewSherpaOnnxProvider(asrConfig)
		if sherpaOnnxProvider == nil {
			return nil, fmt.Errorf("failed to create ASR provider for model %q (model files downloaded?)", cfg.ASR.Model)
		}
		return sherpaOnnxProvider, nil
	})
	asrPool := common.NewModuleProviderPool(cfg.ASR.PoolSize, asrPoolType)
	err = asrPool.Initialize()
	if err != nil {
		log.Fatal(err)
	}

	// tts
	ttsPoolType := reflect.TypeOf(&tts.SherpaOnnxProvider{})
	common.RegisterNewFunc(ttsPoolType, func() (common.IPoolInstance, error) {
		ttsConfig := tts.NewDefaultSherpaOnnxOfflineTtsConfig()
		ttsConfig.Model.NumThreads = cfg.TTS.NumThreads
		sherpaOnnxProvider := tts.NewSherpaOnnxProvider(ttsConfig, cfg.TTS.SpeakerID, cfg.TTS.Speed, cfg.TTS.Model+"TTS")
		if sherpaOnnxProvider == nil {
			return nil, fmt.Errorf("failed to create TTS provider for model %q (model files downloaded?)", cfg.TTS.Model)
		}
		return sherpaOnnxProvider, nil
	})
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
		return llm_processors.NewLLMOpenAIApiProcessor(provider, session, llm_processors.Mode_Chat, cfg.LLM.Stream, *types.NewLMGenerateArgs()), nil
	default:
		return nil, fmt.Errorf("unknown llm.provider %q", cfg.LLM.Provider)
	}
}

// sessionOverrides holds per-connection settings parsed from ws query params.
type sessionOverrides struct {
	voiceID  int
	speed    float32
	llmModel string
	lang     string // "" | en | zh | ja | ko | yue
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
	if m := q.Get("llm"); m != "" && len(m) <= 100 {
		o.llmModel = m
	}
	if l := q.Get("lang"); languageNames[l] != "" {
		o.lang = l
	}
	return o
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
	clientID           string
	systemPrompt       string
	voiceID            int
	speed              float32
	llmModel           string
	addWavHeader       bool    // true for the browser; false for raw telephony audio
	hello              string  // optional greeting synthesized and played on connect
	allowInterruptions bool    // browser: barge-in; telephony: false (half-duplex, echo-safe)
	idlePrompt         string  // spoken after idleSecs of silence ("" disables)
	idleSecs           float64 // silence threshold before idlePrompt fires
	audioOutFrameMS    int     // outbound audio framing; small = lower first-audio latency
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

	runVoiceSession(&ExampleIWebSocketConn{Conn: conn}, serializers.NewProtobufSerializer(), sessionConfig{
		clientID:           fmt.Sprintf("%s_%s", conn.RemoteAddr().Network(), conn.RemoteAddr().String()),
		systemPrompt:       systemPromptFor(overrides.lang),
		voiceID:            overrides.voiceID,
		speed:              overrides.speed,
		llmModel:           overrides.llmModel,
		addWavHeader:       true,
		allowInterruptions: cfg.Server.AllowInterruptions,
	})
}

// sendAudioChunks synthesizes-then-streams pre-rendered PCM to the client as
// ~20 ms AudioRawFrames via the serializer, used to play the greeting before
// the conversation loop starts.
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
		if err := tw.SendPayload(frames.NewAudioRawFrame(pcm[off:end], rate, 1, 2)); err != nil {
			return
		}
	}
}

// runVoiceSession builds and runs the VAD->ASR->LLM->TTS pipeline over the
// given connection and serializer. The browser and telephony transports differ
// only in the serializer, WAV framing, and greeting; everything else is shared.
func runVoiceSession(wsConn common.IWebSocketConn, serializer serializers.Serializer, sc sessionConfig) {
	chatHistorySize := cfg.Server.ChatHistorySize
	session := common.NewSession(sc.clientID, &chatHistorySize)
	session.InitChatMessage(map[string]any{"role": "system", "content": sc.systemPrompt})

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
	asrProvider := asrPoolInstanceInfo.GetInstance().(*asr.SherpaOnnxProvider)
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
	ttsProvider := ttsPoolInstanceInfo.GetInstance().(*tts.SherpaOnnxProvider)
	// Pool instances keep the previous session's voice: reset to config
	// defaults first, then apply overrides (no-ops when unset).
	ttsProvider.SetVoice(cfg.TTS.SpeakerID, cfg.TTS.Speed)
	ttsProvider.SetVoice(sc.voiceID, sc.speed)
	ttsProcessor := achatbot_processors.NewTTSProcessor(ttsProvider)
	outRate, outChannels, outSampleWidth := ttsProvider.GetSampleInfo()
	audioCameraParams.WithAudioOutSampleWidth(outSampleWidth).WithAudioOutSampleRate(outRate).WithAudioOutChannels(outChannels)

	// Play the greeting (if any) before the conversation loop. Safe to
	// synthesize directly here: the pipeline's TTS processor is not yet active.
	if sc.hello != "" {
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
		reflect.TypeOf(&achatbot_frames.TurnEndFrame{}), 4)

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
	vadPool, asrPool, ttsPool = load(cfg)

	// Create HTTP server
	server := &http.Server{
		Addr: cfg.Server.Addr,
	}

	// Set up the WebSocket endpoint with Rate Limiter middleware
	rateLimiter := middleware.NewDefaultRateLimiter().WithEnable(cfg.Server.RateLimitEnabled).WithMaxConns(cfg.Server.MaxConns)
	http.HandleFunc("/api/options", handleOptions)
	http.HandleFunc("/api/tts-preview", handleTTSPreview)

	// Telephony (optional): enabled when TELNYX_API_KEY is set.
	telnyxClient = telnyx.NewClientFromEnv()
	if telnyxClient != nil {
		http.HandleFunc("/api/call", handleCall)
		http.HandleFunc("/telnyx/webhook", handleTelnyxWebhook)
		http.HandleFunc("/telnyx/media", handleTelnyxMedia)
		logger.Info("Telephony enabled", "from", telnyxClient.FromNumber(), "public_url", telnyxClient.PublicURL())
	} else {
		logger.Info("Telephony disabled (TELNYX_API_KEY not set)")
	}

	http.Handle("/", rateLimiter.Middleware(http.HandlerFunc(handleWebSocket)))

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
