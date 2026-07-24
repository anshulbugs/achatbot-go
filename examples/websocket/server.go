package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/weedge/pipeline-go/pkg/frames"
	"github.com/weedge/pipeline-go/pkg/logger"
	"github.com/weedge/pipeline-go/pkg/pipeline"
	"github.com/weedge/pipeline-go/pkg/processors"
	"github.com/weedge/pipeline-go/pkg/processors/aggregators"
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
	"achatbot/pkg/transports"
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
func newLLMProcessor(cfg *config.Config, session *common.Session) (processors.IFrameProcessor, error) {
	switch cfg.LLM.Provider {
	case "ollama_api":
		var thinking *string
		if cfg.LLM.Thinking != "" {
			thinking = &cfg.LLM.Thinking
		}
		provider := llm.NewOllamaAPIProvider(llm.OllamaAPIProviderName, cfg.LLM.Model, cfg.LLM.Stream, thinking, nil, cfg.LLM.Tools)
		if provider == nil {
			return nil, fmt.Errorf("failed to create ollama_api LLM provider for model %q", cfg.LLM.Model)
		}
		return llm_processors.NewLLMOllamaApiProcessor(provider, session, llm_processors.Mode_Chat), nil
	case "openai_api":
		provider := llm.NewOpenAIAPIProvider(llm.OpenAIAPIProviderName, cfg.LLM.BaseURL, cfg.LLM.Model, cfg.LLM.Tools)
		if provider == nil {
			return nil, fmt.Errorf("failed to create openai_api LLM provider for model %q at %s", cfg.LLM.Model, cfg.LLM.BaseURL)
		}
		return llm_processors.NewLLMOpenAIApiProcessor(provider, session, llm_processors.Mode_Chat, cfg.LLM.Stream, *types.NewLMGenerateArgs()), nil
	default:
		return nil, fmt.Errorf("unknown llm.provider %q", cfg.LLM.Provider)
	}
}

// handleWebSocket handles incoming WebSocket connections
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Upgrade the HTTP connection to a WebSocket connection
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Error upgrading to WebSocket: %v", err)
		return
	}
	defer conn.Close()

	// Set Session
	clientId := fmt.Sprintf("%s_%s", conn.RemoteAddr().Network(), conn.RemoteAddr().String())
	chatHistorySize := cfg.Server.ChatHistorySize
	session := common.NewSession(clientId, &chatHistorySize)
	session.InitChatMessage(map[string]any{"role": "system", "content": cfg.Server.SystemPrompt})

	// vad provider
	vadPoolInstanceInfo, err := vadPool.Get()
	if err != nil {
		log.Printf("Get VAD instance from pool err: %v", err)
		return
	}
	defer vadPool.Put(vadPoolInstanceInfo)
	vadProvider := vadPoolInstanceInfo.GetInstance().(*vad_analyzer.SherpaOnnxProvider)
	vadAnalyzer := vad_analyzer.NewVADAnalyzer(params.NewVADAnalyzerArgs(), vadProvider)

	// Wrap the connection to implement our interface
	wsConn := &ExampleIWebSocketConn{Conn: conn}

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
		Serializer:        serializers.NewProtobufSerializer(),
	}
	wsParams.WithAudioOutFrameMS(200).WithAudioOutAddWavHeader(true) //200ms + wav head

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
	asrProcessor := achatbot_processors.NewASRProcessor(asrProvider)

	// Set TTS Processor
	ttsPoolInstanceInfo, err := ttsPool.Get()
	if err != nil {
		log.Printf("Get tts instance from pool err: %v", err)
		return
	}
	defer ttsPool.Put(ttsPoolInstanceInfo)
	ttsProvider := ttsPoolInstanceInfo.GetInstance().(*tts.SherpaOnnxProvider)
	ttsProcessor := achatbot_processors.NewTTSProcessor(ttsProvider)
	outRate, outChannels, outSampleWidth := ttsProvider.GetSampleInfo()
	audioCameraParams.WithAudioOutSampleWidth(outSampleWidth).WithAudioOutSampleRate(outRate).WithAudioOutChannels(outChannels)

	// Set LLM Processor from config
	llmProcessor, err := newLLMProcessor(cfg, session)
	if err != nil {
		log.Printf("Create LLM processor err: %v", err)
		return
	}

	// Set Sentence Processor
	sentenceProcessor := aggregators.NewSentenceAggregatorWithEnd(reflect.TypeOf(&achatbot_frames.TurnEndFrame{}))

	// 1. Create the WebSocket server input processor
	ws_transport := transports.NewWebsocketTransport(
		wsConn,
		wsParams,
	)

	// 2. Create a simple pipeline with the async processor
	myPipeline := pipeline.NewPipelineWithVerbose(
		[]processors.IFrameProcessor{
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
			//achatbot_processors.NewAudioSaveProcessor("user_speak", consts.RECORDS_DIR, true),
			asrProcessor.WithPassRawAudio(false),
			processors.NewDefaultFrameLoggerProcessorWithIncludeFrame([]frames.Frame{&frames.TextFrame{}}),
			llmProcessor,
			//processors.NewDefaultFrameLoggerProcessorWithIncludeFrame([]frames.Frame{&achatbot_frames.ThinkTextFrame{}, &frames.TextFrame{}}),
			sentenceProcessor,
			processors.NewDefaultFrameLoggerProcessorWithIncludeFrame([]frames.Frame{&frames.TextFrame{}}),
			ttsProcessor.WithPassText(true),
			processors.NewDefaultFrameLoggerProcessorWithIncludeFrame([]frames.Frame{&frames.AudioRawFrame{}}),
			//achatbot_processors.NewAudioResampleProcessor(audioCameraParams.AudioOutSampleRate),
			//processors.NewDefaultFrameLoggerProcessorWithIncludeFrame([]frames.Frame{&frames.AudioRawFrame{}}),
			//achatbot_processors.NewAudioSaveProcessor("bot_speak", consts.RECORDS_DIR, true),
			processors.NewDefaultFrameLoggerProcessorWithIncludeFrame([]frames.Frame{&frames.AudioRawFrame{}}),
			ws_transport.OutputProcessor(),
		},
		nil, nil,
		false,
	)
	logger.Info(myPipeline.String())

	// In a real application, you would integrate this with your frame processing pipeline
	// and properly manage the processor lifecycle
	// 3. Create and run a pipeline task
	// NOTE: set IsPushBlock: false, IsUpPushBlock: false to debug queue frame and check slow process
	task := pipeline.NewPipelineTask(myPipeline, pipeline.PipelineParams{
		AllowInterruptions: cfg.Server.AllowInterruptions,
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
