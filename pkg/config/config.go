// Package config provides file/env-driven configuration for the achatbot
// pipeline: which VAD/ASR/TTS/LLM providers and models to run, provider pool
// sizes, and server settings. Zero configuration reproduces the previously
// hardcoded example-server behavior, so existing setups keep working.
//
// Precedence (highest wins): ACHATBOT_* environment variables > config file >
// built-in defaults. The config file is either passed explicitly to Load or
// discovered as config.yaml in the working directory or ./configs.
package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"

	"achatbot/pkg/consts"
)

// ErrInvalidConfig wraps every validation failure so callers can match with errors.Is.
var ErrInvalidConfig = errors.New("invalid config")

const envPrefix = "ACHATBOT"

// Provider/model name sets accepted by validation. These must stay in sync
// with the switchable implementations in pkg/modules.
var (
	ValidVADModels    = []string{"silero", "ten"}
	ValidASRModels    = []string{"sense_voice", "whisper", "paraformer", "zipformer_ctc", "moonshine", "fire_red_asr", "dolphin", "nemo_ctc", "parakeet_http"}
	ValidTTSModels    = []string{"kokoro", "kokoro_http", "voxtral_http", "kani_http", "dots_http"}
	ValidLLMProviders = []string{"openai_api", "ollama_api"}
	ValidThinking     = []string{"", "low", "medium", "high"}
)

// Config is the root configuration for a voice-agent server process.
type Config struct {
	Server ServerConfig `mapstructure:"server"`
	VAD    VADConfig    `mapstructure:"vad"`
	ASR    ASRConfig    `mapstructure:"asr"`
	TTS    TTSConfig    `mapstructure:"tts"`
	LLM    LLMConfig    `mapstructure:"llm"`
}

// ServerConfig controls the websocket HTTP server and per-session chat settings.
type ServerConfig struct {
	// Addr is the HTTP listen address, e.g. ":4321".
	Addr string `mapstructure:"addr"`
	// MaxConns is the rate-limiter connection cap per IP.
	MaxConns int `mapstructure:"max_conns"`
	// RateLimitEnabled toggles the per-IP connection rate limiter.
	RateLimitEnabled bool `mapstructure:"rate_limit_enabled"`
	// ChatHistorySize is the number of chat turns kept per session.
	ChatHistorySize int `mapstructure:"chat_history_size"`
	// AllowInterruptions enables barge-in: when the user starts speaking the
	// pipeline cancels in-flight LLM generation and drops queued TTS audio.
	AllowInterruptions bool `mapstructure:"allow_interruptions"`
	// SystemPrompt seeds every session's system message.
	SystemPrompt string `mapstructure:"system_prompt"`
	// IdlePromptSecs, when > 0, makes the agent speak IdlePromptText after
	// this many seconds of silence (no bot or caller audio). 0 disables it.
	IdlePromptSecs float64 `mapstructure:"idle_prompt_secs"`
	// IdlePromptText is spoken on idle timeout (e.g. "Are you still there?").
	IdlePromptText string `mapstructure:"idle_prompt_text"`
	// TurnGateEnabled inserts a semantic-endpointing LLM between ASR and the
	// main LLM: it decides whether the caller has finished (respond) or is
	// pausing (wait), and cleans up STT errors. Lets vad.stop_secs stay short.
	TurnGateEnabled bool `mapstructure:"turn_gate_enabled"`
	// TurnGateModel is the small/fast model used for the gate decision.
	TurnGateModel string `mapstructure:"turn_gate_model"`
	// TurnGateMaxWaitSecs is the absolute silence after which a held
	// (incomplete) utterance is forwarded anyway, so the gate can never hang.
	TurnGateMaxWaitSecs float64 `mapstructure:"turn_gate_max_wait_secs"`
	// FirstChunkWords is how many opening words of a reply to flush to TTS
	// immediately (smaller = audio starts sooner, but choppier openings).
	FirstChunkWords int `mapstructure:"first_chunk_words"`
	// AvatarURL is the video-avatar service the browser connects to, e.g.
	// wss://<tunnel-host>. It lives in config rather than the page because the
	// tunnel hostname changes every time the tunnel restarts, and baking it into
	// the markup meant a stale address survived in browsers as "bad url".
	// Must be wss:// when the UI is served over HTTPS; browsers block plain ws://
	// from a secure page.
	AvatarURL string `mapstructure:"avatar_url"`
	// InboundHello is the greeting spoken when the agent answers an inbound call.
	// Falls back to a generic greeting when empty.
	InboundHello string `mapstructure:"inbound_hello"`
	// MaxCallSecs hangs a call up after this many seconds. Needed for agent-to-agent
	// load tests, where neither side ever hangs up. 0 disables the cap.
	MaxCallSecs int `mapstructure:"max_call_secs"`
	// RecordCalls asks Telnyx to record every answered call (dual channel, mp3).
	// Recordings are billed and stored by Telnyx — keep this off for large runs.
	RecordCalls bool `mapstructure:"record_calls"`
	// ClarityFilter enables the outbound telephone voice-enhancement filter
	// (high-pass + presence boost) on Telnyx calls.
	ClarityFilter bool `mapstructure:"clarity_filter"`
}

// VADConfig selects the voice-activity-detection model and its provider pool.
type VADConfig struct {
	// Model is the VAD model name: silero or ten.
	Model string `mapstructure:"model"`
	// PoolSize is how many VAD provider instances to preload; each active
	// connection holds one, so this bounds concurrent sessions.
	PoolSize int `mapstructure:"pool_size"`
	// BufferSizeSeconds is the sherpa-onnx VAD ring-buffer length.
	BufferSizeSeconds float32 `mapstructure:"buffer_size_seconds"`
	// NumThreads is the onnxruntime intra-op thread count per instance.
	NumThreads int `mapstructure:"num_threads"`
	// StartSecs is how much sustained speech marks a turn start.
	StartSecs float64 `mapstructure:"start_secs"`
	// StopSecs is how much trailing silence ends the user's turn. Lower is
	// snappier but risks cutting slow speakers off mid-sentence.
	StopSecs float64 `mapstructure:"stop_secs"`
}

// ASRConfig selects the speech-recognition model and its provider pool.
type ASRConfig struct {
	// Model is the ASR model name; see ValidASRModels. Model files must be
	// downloaded under models/ as documented in the README.
	Model string `mapstructure:"model"`
	// PoolSize is how many ASR provider instances to preload.
	PoolSize int `mapstructure:"pool_size"`
	// NumThreads is the onnxruntime intra-op thread count per instance;
	// raising it shortens transcription latency on multi-core hosts.
	NumThreads int `mapstructure:"num_threads"`
	// Language forces the recognized language for models that support it
	// (currently sense_voice: "en", "zh", "ja", "ko", "yue"). Empty means
	// auto-detect. Forcing the expected language avoids mis-detection on
	// short or noisy utterances.
	Language string `mapstructure:"language"`
	// HTTPURL is the base URL of a GPU ASR service (used when Model is
	// "parakeet_http"), e.g. http://127.0.0.1:8890.
	HTTPURL string `mapstructure:"http_url"`
}

// TTSConfig selects the speech-synthesis model, voice, and provider pool.
type TTSConfig struct {
	// Model is the TTS model name; only kokoro is implemented today.
	Model string `mapstructure:"model"`
	// SpeakerID selects the voice (kokoro multi-speaker: 45-52 are zh voices).
	SpeakerID int `mapstructure:"speaker_id"`
	// Speed scales speech rate; larger is faster, must be > 0.
	Speed float32 `mapstructure:"speed"`
	// PoolSize is how many TTS provider instances to preload.
	PoolSize int `mapstructure:"pool_size"`
	// NumThreads is the onnxruntime intra-op thread count per instance;
	// raising it shortens synthesis latency (the dominant per-reply cost).
	NumThreads int `mapstructure:"num_threads"`
	// HTTPURL is the base URL of a GPU TTS service (used when Model is
	// "kokoro_http"), e.g. http://127.0.0.1:8880.
	HTTPURL string `mapstructure:"http_url"`
	// Gain scales output loudness (1.0 = unchanged). Clipped to int16; keep
	// modest (<=1.6) to avoid distortion on the telephone codec.
	Gain float32 `mapstructure:"gain"`
	// HTTPVoice is the voice name for OpenAI-speech-style GPU TTS services
	// (used when Model is "voxtral_http"), e.g. "casual_female".
	HTTPVoice string `mapstructure:"http_voice"`
}

// LLMConfig selects the language-model provider, endpoint, and generation mode.
type LLMConfig struct {
	// Provider is openai_api (any OpenAI-compatible endpoint, including
	// Ollama's /v1 and OpenRouter) or ollama_api (native Ollama client).
	Provider string `mapstructure:"provider"`
	// BaseURL is the OpenAI-compatible endpoint; required for openai_api,
	// ignored by ollama_api (which uses OLLAMA_HOST).
	BaseURL string `mapstructure:"base_url"`
	// Model is the model identifier as the provider knows it.
	Model string `mapstructure:"model"`
	// Stream enables token streaming.
	Stream bool `mapstructure:"stream"`
	// Thinking sets reasoning effort for ollama_api: low, medium, high, or
	// empty to disable.
	Thinking string `mapstructure:"thinking"`
	// Tools lists registered function names the LLM may call, e.g. web_search.
	Tools []string `mapstructure:"tools"`
	// Temperature controls randomness. Voice agents follow a long instruction
	// set, so this stays low; raising it is what makes a model start inventing
	// names and details it cannot account for.
	Temperature float64 `mapstructure:"temperature"`
	// MaxTokens caps a single reply. The default of 2048 is far more than
	// anyone will listen to on a call, and an over-long generation holds a
	// decode slot for its whole length, which costs concurrency. A spoken
	// sentence is roughly 15-20 tokens.
	MaxTokens int64 `mapstructure:"max_tokens"`
}

// Load reads configuration from the optional explicit YAML path, discovered
// config.yaml (working directory or ./configs), and ACHATBOT_* environment
// variables, then validates it. A missing explicit path is an error; a missing
// discovered file falls back to defaults. All returned errors from validation
// wrap ErrInvalidConfig.
func Load(path string) (*Config, error) {
	v := viper.New()
	setDefaults(v)

	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("./configs")
		if err := v.ReadInConfig(); err != nil {
			var notFound viper.ConfigFileNotFoundError
			if !errors.As(err, &notFound) {
				return nil, fmt.Errorf("read config: %w", err)
			}
		}
	}

	cfg := &Config{}
	decode := func(dc *mapstructure.DecoderConfig) { dc.WeaklyTypedInput = true }
	if err := v.Unmarshal(cfg, decode); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.addr", ":4321")
	v.SetDefault("server.max_conns", 3)
	v.SetDefault("server.rate_limit_enabled", true)
	v.SetDefault("server.chat_history_size", 2)
	v.SetDefault("server.allow_interruptions", false)
	v.SetDefault("server.system_prompt", consts.DefaultLLMSystemPrompt)
	v.SetDefault("server.idle_prompt_secs", 0)
	v.SetDefault("server.idle_prompt_text", "Are you still there?")
	v.SetDefault("server.turn_gate_enabled", false)
	v.SetDefault("server.turn_gate_model", "qwen3:0.6b")
	v.SetDefault("server.turn_gate_max_wait_secs", 2.5)
	v.SetDefault("server.first_chunk_words", 4)
	v.SetDefault("server.clarity_filter", true)

	v.SetDefault("vad.model", "silero")
	v.SetDefault("vad.pool_size", 3)
	v.SetDefault("vad.buffer_size_seconds", 100)
	v.SetDefault("vad.num_threads", 1)
	v.SetDefault("vad.start_secs", 0.032)
	v.SetDefault("vad.stop_secs", 0.32)

	v.SetDefault("asr.model", "sense_voice")
	v.SetDefault("asr.pool_size", 1)
	v.SetDefault("asr.num_threads", 1)
	v.SetDefault("asr.language", "")
	v.SetDefault("asr.http_url", "http://127.0.0.1:8890")

	v.SetDefault("tts.model", "kokoro")
	v.SetDefault("tts.speaker_id", 49) // kokoro zm_yunjian
	v.SetDefault("tts.speed", 1.0)
	v.SetDefault("tts.pool_size", 1)
	v.SetDefault("tts.num_threads", 1)
	v.SetDefault("tts.http_url", "http://127.0.0.1:8880")
	v.SetDefault("tts.gain", 1.0)
	v.SetDefault("tts.http_voice", "casual_female")

	v.SetDefault("llm.provider", "openai_api")
	v.SetDefault("llm.base_url", "http://127.0.0.1:11434/v1")
	v.SetDefault("llm.model", "qwen3:0.6b")
	v.SetDefault("llm.stream", true)
	v.SetDefault("llm.thinking", "")
	v.SetDefault("llm.tools", []string{"web_search"})
	v.SetDefault("llm.temperature", 0.6)
	v.SetDefault("llm.max_tokens", 160)
}

func (c *Config) validate() error {
	if !slices.Contains(ValidVADModels, c.VAD.Model) {
		return invalidf("vad.model %q not in %v", c.VAD.Model, ValidVADModels)
	}
	if !slices.Contains(ValidASRModels, c.ASR.Model) {
		return invalidf("asr.model %q not in %v", c.ASR.Model, ValidASRModels)
	}
	if !slices.Contains(ValidTTSModels, c.TTS.Model) {
		return invalidf("tts.model %q not in %v", c.TTS.Model, ValidTTSModels)
	}
	if !slices.Contains(ValidLLMProviders, c.LLM.Provider) {
		return invalidf("llm.provider %q not in %v", c.LLM.Provider, ValidLLMProviders)
	}
	if !slices.Contains(ValidThinking, c.LLM.Thinking) {
		return invalidf("llm.thinking %q not in %v", c.LLM.Thinking, ValidThinking[1:])
	}
	if c.LLM.Thinking != "" && c.LLM.Provider != "ollama_api" {
		return invalidf("llm.thinking is only supported when llm.provider is ollama_api, got %q", c.LLM.Provider)
	}
	if c.VAD.PoolSize < 1 {
		return invalidf("vad.pool_size %d must be >= 1", c.VAD.PoolSize)
	}
	if c.ASR.PoolSize < 1 {
		return invalidf("asr.pool_size %d must be >= 1", c.ASR.PoolSize)
	}
	if c.TTS.PoolSize < 1 {
		return invalidf("tts.pool_size %d must be >= 1", c.TTS.PoolSize)
	}
	if c.VAD.BufferSizeSeconds <= 0 {
		return invalidf("vad.buffer_size_seconds %v must be > 0", c.VAD.BufferSizeSeconds)
	}
	if c.VAD.NumThreads < 1 {
		return invalidf("vad.num_threads %d must be >= 1", c.VAD.NumThreads)
	}
	if c.VAD.StartSecs <= 0 || c.VAD.StartSecs > 2 {
		return invalidf("vad.start_secs %v must be in (0, 2]", c.VAD.StartSecs)
	}
	if c.VAD.StopSecs <= 0 || c.VAD.StopSecs > 5 {
		return invalidf("vad.stop_secs %v must be in (0, 5]", c.VAD.StopSecs)
	}
	if c.ASR.NumThreads < 1 {
		return invalidf("asr.num_threads %d must be >= 1", c.ASR.NumThreads)
	}
	if c.TTS.NumThreads < 1 {
		return invalidf("tts.num_threads %d must be >= 1", c.TTS.NumThreads)
	}
	if c.TTS.Speed <= 0 {
		return invalidf("tts.speed %v must be > 0", c.TTS.Speed)
	}
	if c.TTS.SpeakerID < 0 {
		return invalidf("tts.speaker_id %d must be >= 0", c.TTS.SpeakerID)
	}
	if c.LLM.Model == "" {
		return invalidf("llm.model must not be empty")
	}
	if c.LLM.Provider == "openai_api" && c.LLM.BaseURL == "" {
		return invalidf("llm.base_url must not be empty when llm.provider is openai_api")
	}
	if c.Server.ChatHistorySize < 0 {
		return invalidf("server.chat_history_size %d must be >= 0", c.Server.ChatHistorySize)
	}
	return nil
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
}
