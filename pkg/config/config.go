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
	ValidASRModels    = []string{"sense_voice", "whisper", "paraformer", "zipformer_ctc", "moonshine", "fire_red_asr", "dolphin", "nemo_ctc"}
	ValidTTSModels    = []string{"kokoro"}
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

	v.SetDefault("vad.model", "silero")
	v.SetDefault("vad.pool_size", 3)
	v.SetDefault("vad.buffer_size_seconds", 100)
	v.SetDefault("vad.num_threads", 1)

	v.SetDefault("asr.model", "sense_voice")
	v.SetDefault("asr.pool_size", 1)
	v.SetDefault("asr.num_threads", 1)

	v.SetDefault("tts.model", "kokoro")
	v.SetDefault("tts.speaker_id", 49) // kokoro zm_yunjian
	v.SetDefault("tts.speed", 1.0)
	v.SetDefault("tts.pool_size", 1)
	v.SetDefault("tts.num_threads", 1)

	v.SetDefault("llm.provider", "openai_api")
	v.SetDefault("llm.base_url", "http://127.0.0.1:11434/v1")
	v.SetDefault("llm.model", "qwen3:0.6b")
	v.SetDefault("llm.stream", true)
	v.SetDefault("llm.thinking", "")
	v.SetDefault("llm.tools", []string{"web_search"})
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
