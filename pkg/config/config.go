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
	"strconv"
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
	// LiveRoomPrepublish decides HOW an operator's Join reaches the agent, and
	// the two modes cannot both be on — the dialer's own code makes it either/or.
	//
	// true (default, and what shipped): the listen-in room is created when the
	// call is dispatched and its link published to Redis straight away. The
	// dialer stashes that link and then SKIPS its own
	// GET {join_call_url}?uuid=... on Join, because it already has a room
	// (rexa-dialer calls.py `_bridge_daily_room`). With no request to answer,
	// the agent can only notice the operator through Daily's presence API,
	// which lags a real join by about 5.6 seconds — so barging takes ~10s.
	//
	// false: nothing is published up front, the dialer's fast path misses, and
	// it calls /join-call on the click. Bridging starts immediately and barging
	// takes ~2s. A room is also only created for calls somebody actually
	// opened, rather than one per dispatch.
	//
	// Set false ONLY once rexa-dialer's JOIN_CALL_URL points at this agent.
	// Until it does, false means Join finds no room at all.
	LiveRoomPrepublish bool `mapstructure:"live_room_prepublish"`
	// LiveRoomPrewarm holds a listen-in room's session open from the moment the
	// call is answered, so its SIP endpoint is registered before anyone barges.
	//
	// Daily registers a room's SIP endpoint on SESSION start, not on room
	// creation, and a session needs a WebRTC participant. Until this existed
	// that participant was the supervisor themselves, so their barge waited out
	// registration in full: 4.8-5.3s measured on every one, and by far the
	// largest part of the delay. A silent pre-joiner moves that wait into the
	// conversation, where nobody is watching a clock. Registration completed
	// 1.9s after its join when measured.
	//
	// THE COST IS A DAILY PARTICIPANT for the length of every answered call
	// with a room, whether or not anyone ever barges — so it is worth checking
	// against your participant-minute rate before leaving it on. The process
	// itself is cheap: ~34MB, no GPU.
	//
	// Off by default. Needs SIDECAR_PYTHON and PREWARM_SCRIPT set.
	LiveRoomPrewarm bool `mapstructure:"live_room_prewarm"`
	// JoinCallFallbackURL is where /join-call forwards a uuid this agent does
	// not know.
	//
	// WHAT MAKES THE SWITCH SAFE. The platform has one join_call_url, and it
	// currently points at the legacy agent's sidecar — so repointing it here
	// would break Join for every call that agent still owns. With a fallback
	// set, this endpoint answers for its own calls and proxies the rest
	// unchanged, so both agents keep working through one URL and the platform
	// never has to know which box owns a given call.
	//
	// Empty disables proxying and an unknown uuid gets a plain 404.
	JoinCallFallbackURL string `mapstructure:"join_call_fallback_url"`
	// DialTimeoutSecs is how long an outbound call is allowed to ring before
	// the carrier gives up and reports no_answer.
	//
	// THIS IS A VOICEMAIL SETTING as much as a ring setting. Most carriers
	// divert to voicemail somewhere between 25 and 35 seconds, so Telnyx's
	// default of 30 sits exactly on that boundary: a line whose voicemail
	// would have answered at 32s is abandoned at 30 and reported no_answer,
	// and no message is left because nothing ever picked up. Raise it above
	// the divert time to reach voicemail reliably; lower it to spend less
	// carrier time on numbers that will not answer.
	DialTimeoutSecs int `mapstructure:"dial_timeout_secs"`
	// VoicemailDetection enables Telnyx answering-machine detection on outbound
	// calls: disabled, detect, detect_beep, detect_words, greeting_end, premium.
	// When a machine is detected the AI pipeline is torn down immediately, which
	// hands back that call's VAD/ASR/TTS slots -- a voicemail otherwise holds a
	// full pipeline for the whole message while nobody is listening.
	VoicemailDetection string `mapstructure:"voicemail_detection"`
	// VoicemailMessage, when set, is spoken into the voicemail using Telnyx's
	// own TTS after the pipeline is released, so leaving a message costs no GPU.
	// Empty means hang up as soon as a machine is detected.
	VoicemailMessage string `mapstructure:"voicemail_message"`
	// RecordCalls asks Telnyx to record every answered call (dual channel, mp3).
	// Recordings are billed and stored by Telnyx — keep this off for large runs.
	RecordCalls bool `mapstructure:"record_calls"`
	// EvalEnabled turns on POST /evaluate, the end-of-call evaluation endpoint.
	//
	// Off by default. It runs the SAME LLM the calls use, so it is a deliberate
	// decision to share that capacity, not something a deployment should
	// acquire by upgrading.
	EvalEnabled bool `mapstructure:"eval_enabled"`
	// EvalConcurrency is how many evaluations may run at once. Small on
	// purpose: a burst of hangups must not become a burst of transcript-sized
	// prefills in front of live callers. Zero means 2.
	EvalConcurrency int `mapstructure:"eval_concurrency"`
	// EvalMaxWaitSecs is how long an evaluation waits for a quiet box before
	// running anyway. Seconds, not minutes — a request that waits forever is a
	// broken feature, and past this point the concurrency cap is what bounds
	// the cost. Zero means 20.
	EvalMaxWaitSecs int `mapstructure:"eval_max_wait_secs"`
	// ClarityFilter enables the outbound telephone voice-enhancement filter
	// (high-pass + presence boost) on Telnyx calls.
	ClarityFilter bool `mapstructure:"clarity_filter"`
	// MaxGPUCalls is the concurrency ceiling the platform contract enforces:
	// past this many calls holding a pipeline, /connection is refused with
	// at_capacity and /health reports accepting=false. Calls playing a
	// voicemail announcement do NOT count — they hold no pool slots.
	//
	// 0 means unlimited. This is a MEASURED value, not a guess: 60 concurrent
	// agent sessions held p95 at 1628ms with zero dropped audio writes, while
	// 100 produced 6244ms and 234 drops. Remeasure with deploy/loadtest
	// whenever the model, prompt size or GPU layout changes — it is specific
	// to all three.
	MaxGPUCalls int `mapstructure:"max_gpu_calls"`
	// MaxTotalCalls is the absolute in-flight ceiling, counting calls that
	// cost no GPU at all. It stands in for the limits our own counters cannot
	// see -- the carrier's concurrent channel cap, CPU for hundreds of media
	// streams, TTS renders at dispatch -- and no capacity estimate may argue
	// past it. 0 = unlimited.
	MaxTotalCalls int `mapstructure:"max_total_calls"`
	// HumanAnswerWeight is the expected GPU cost of one dispatched call: the
	// fraction that reach a live pipeline rather than an answering machine, a
	// no-answer or a busy.
	//
	// Accepts a number, or the literal "auto".
	//
	// The governing rule is `weight >= actual answer rate`. Admission allows
	// (max_gpu_calls / weight) calls to be ringing at once, and the fraction
	// of those that answer become pipelines, so a weight below the real rate
	// creates more pipelines than the ceiling allows -- and a call a human has
	// already picked up cannot be refused.
	//
	//   1.0    every dispatch charged a full slot. The ONLY setting that
	//          guarantees on_gpu never exceeds max_gpu_calls, since converting
	//          a reservation to a pipeline leaves the total unchanged.
	//          Under-utilises when most calls hit voicemail.
	//   auto   track the measured answer rate, times a safety factor, floored
	//          and capped. Starts at 1.0 until enough calls have resolved.
	//   0.3    a fixed over-subscription, for when the rate is known and steady.
	//
	// Use 1.0 for a first campaign, read measured.answer_rate off /dashboard,
	// then decide. Resolve it with ResolveHumanAnswerWeight.
	HumanAnswerWeight string `mapstructure:"human_answer_weight"`
	// TransferCallerID chooses the caller ID presented to a transfer
	// destination: "contact" (default) shows the number of the person being
	// transferred, "tenant" shows the number we dialled from.
	//
	// "contact" is what the receiving human wants to see -- it is the point of
	// transferring rather than cold-dialling them. The cost is that a `from`
	// which is not on the carrier account gets low or no STIR/SHAKEN
	// attestation, and US carriers increasingly mark those "Spam Likely" or
	// drop them. Suspect this setting first if transfers connect unreliably.
	// `from_display_name` carries the contact's name under either setting.
	TransferCallerID string `mapstructure:"transfer_caller_id"`
	// FirstTurnSaturatedMs is the p95 first-token latency of a call's FIRST
	// reply at which /health stops accepting work, in milliseconds.
	//
	// First turn is tracked apart from every other turn because it is the only
	// one that pays a cold prefill, so it is where KV-cache pressure appears
	// first and largest: measured at 60 concurrent calls, sharing one campaign
	// prompt gave 1853 ms while a distinct prompt per call gave 9903 ms — a
	// pooled average of all turns read 6252 ms and would not have fired.
	//
	// This can refuse traffic on its own, with plenty of room left under
	// max_gpu_calls. That is deliberate: 61 was measured under one prompt size
	// and one degree of prefix sharing, and a heavier workload invalidates it.
	// Serving six calls well beats accepting sixty-one and serving all of them
	// badly. 0 uses the built-in default.
	FirstTurnSaturatedMs int `mapstructure:"first_turn_saturated_ms"`
	// FirstTurnCriticalMs trips the gate on a SINGLE first turn this slow,
	// without waiting for enough samples to form a percentile. Ten calls with
	// ten unrelated prompts produce ten samples in total; waiting to be
	// statistically comfortable means answering "send more" twice before
	// reacting. 0 uses the built-in default.
	FirstTurnCriticalMs int `mapstructure:"first_turn_critical_ms"`
	// FirstTurnCooldownSecs is how long the gate stays shut after tripping.
	//
	// A duty cycle, not a latch. The measurement is fed only by new calls, so a
	// gate that stayed shut until the numbers recovered would cut off the
	// samples that could show recovery and never reopen. 0 uses the default.
	FirstTurnCooldownSecs int `mapstructure:"first_turn_cooldown_secs"`
	// ForceVoiceID pins every platform-dispatched call to one TTS speaker,
	// ignoring the voice the dispatch asked for. -1 honours the platform.
	//
	// Blunt on purpose. While the voice is still being chosen, a different
	// speaker per call — because one tenant's vocabulary happens to map
	// somewhere and another's does not — makes every other judgement harder:
	// pacing, prompt wording, whether a greeting sounds natural.
	ForceVoiceID int `mapstructure:"force_voice_id"`
	// SentimentBaseURL is an OpenAI-compatible endpoint used ONLY for mid-call
	// sentiment classification. Empty disables the feature regardless of what
	// a dispatch asks for.
	//
	// Point this at a SMALL model on its own endpoint, never at the
	// conversation LLM. First-turn latency on that model is what decides how
	// many calls the fleet can carry, and classifying on it spends the exact
	// resource capacity is measured in. A 0.6B model costs nothing the caller
	// waits for and nothing the capacity gate counts.
	SentimentBaseURL string `mapstructure:"sentiment_base_url"`
	// SentimentModel is the model name at SentimentBaseURL.
	//
	// Must be a NON-REASONING model. qwen3:0.6b was the obvious pick on size
	// and is wrong here: it emits <think> before answering, so a small
	// max_tokens truncates the reply to nothing at all, and giving it room to
	// think costs ~220 tokens a turn and still scored 4/10 — with both errors
	// false "wants_human", which pages a human over a salary question.
	// llama3.2:3b answers in one word, scored 10/10 on the same probes, and
	// takes ~420 ms.
	SentimentModel string `mapstructure:"sentiment_model"`
	// SGLangMetricsURLs are the SGLang server base URLs (NOT the /v1 path)
	// whose /metrics endpoint is polled in the background for cache hit rate
	// and queue depth. Reported on /health and /dashboard, never acted on.
	// Empty disables polling.
	SGLangMetricsURLs []string `mapstructure:"sglang_metrics_urls"`
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
	// Markup enables kokoro's inline speech markup: "[only](+1)" to raise the
	// stress on a word and "[JobTalk](/ʤˈɑbtˌɔk/)" to force a pronunciation.
	//
	// Only meaningful for the kokoro path, whose misaki front end parses it.
	// Turning it on also tells the model it may use stress marks; leaving it
	// off strips any that appear rather than forwarding them.
	Markup bool `mapstructure:"markup"`
	// Pronunciations forces how specific words are said, as word -> misaki
	// phonemes (slashes optional). It is applied deterministically just before
	// synthesis, so a brand or a contact name sounds the same on every turn of
	// every call without the model having to remember it.
	//
	// The alphabet is misaki's, not plain IPA: affricates are the single
	// characters "ʤ" and "ʧ", and the diphthongs are written A I O W Y.
	// Requires Markup.
	Pronunciations map[string]string `mapstructure:"pronunciations"`
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
	// Registering a default is what makes ACHATBOT_SERVER_MAX_GPU_CALLS work
	// at all: viper's AutomaticEnv only resolves keys it already knows from a
	// default or the config file when Unmarshal runs, so a key present in
	// neither is silently invisible to the environment. 0 = unlimited.
	v.SetDefault("server.max_gpu_calls", 0)
	v.SetDefault("server.max_total_calls", 0)
	v.SetDefault("server.human_answer_weight", "1.0")
	v.SetDefault("server.transfer_caller_id", "contact")
	// 0 means "use the package default" rather than "disabled": a first-turn
	// gate that switched itself off when unconfigured would be off everywhere
	// it matters most.
	v.SetDefault("server.first_turn_saturated_ms", 0)
	v.SetDefault("server.first_turn_critical_ms", 0)
	v.SetDefault("server.first_turn_cooldown_secs", 0)
	v.SetDefault("server.sglang_metrics_urls", []string{})
	v.SetDefault("server.force_voice_id", -1)
	v.SetDefault("server.sentiment_base_url", "")
	v.SetDefault("server.sentiment_model", "llama3.2:3b")
	v.SetDefault("server.idle_prompt_secs", 0)
	v.SetDefault("server.idle_prompt_text", "Are you still there?")
	v.SetDefault("server.turn_gate_enabled", false)
	v.SetDefault("server.turn_gate_model", "qwen3:0.6b")
	v.SetDefault("server.turn_gate_max_wait_secs", 2.5)
	v.SetDefault("server.first_chunk_words", 4)
	v.SetDefault("server.voicemail_detection", "disabled")
	// Telnyx's own default, so an unset key changes nothing.
	v.SetDefault("server.dial_timeout_secs", 30)
	// Safe by default: Join keeps working without any change on the platform
	// side, at the cost of the slow path.
	v.SetDefault("server.live_room_prepublish", true)
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

// AdaptiveAnswerWeight is the internal sentinel meaning "compute the weight
// from measurements". Config expresses it as the word "auto"; a bare 0 in the
// file is rejected, because a weight of zero would otherwise read as "a
// ringing call costs nothing", which is not what it means.
const AdaptiveAnswerWeight = 0.0

// ResolveHumanAnswerWeight turns the configured value into the number the
// metrics registry takes: a weight in (0,1], or AdaptiveAnswerWeight for
// "auto".
func (c *ServerConfig) ResolveHumanAnswerWeight() (float64, error) {
	raw := strings.TrimSpace(strings.ToLower(c.HumanAnswerWeight))
	if raw == "" {
		return 1.0, nil
	}
	if raw == "auto" {
		return AdaptiveAnswerWeight, nil
	}
	w, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, invalidf("server.human_answer_weight must be a number or \"auto\", got %q", c.HumanAnswerWeight)
	}
	if w <= 0 {
		return 0, invalidf("server.human_answer_weight must be > 0 (use \"auto\" for the measured rate), got %s", c.HumanAnswerWeight)
	}
	if w > 1 {
		return 0, invalidf("server.human_answer_weight must be <= 1 (a ringing call cannot cost more than a live one), got %s", c.HumanAnswerWeight)
	}
	return w, nil
}

func (c *Config) validate() error {
	// Resolve for its side effect: a typo here must fail at boot, not silently
	// fall back to a capacity policy nobody chose.
	if _, err := c.Server.ResolveHumanAnswerWeight(); err != nil {
		return err
	}
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
