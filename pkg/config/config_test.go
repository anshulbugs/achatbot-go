package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadDefaults verifies that with no config file present, Load returns
// defaults that exactly match the previously hardcoded server behavior.
func TestLoadDefaults(t *testing.T) {
	t.Chdir(t.TempDir())

	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, ":4321", cfg.Server.Addr)
	assert.Equal(t, 3, cfg.Server.MaxConns)
	assert.True(t, cfg.Server.RateLimitEnabled)
	assert.Equal(t, 2, cfg.Server.ChatHistorySize)
	assert.False(t, cfg.Server.AllowInterruptions)
	assert.NotEmpty(t, cfg.Server.SystemPrompt)

	assert.Equal(t, "silero", cfg.VAD.Model)
	assert.Equal(t, 3, cfg.VAD.PoolSize)
	assert.Equal(t, float32(100), cfg.VAD.BufferSizeSeconds)

	assert.Equal(t, "sense_voice", cfg.ASR.Model)
	assert.Equal(t, 1, cfg.ASR.PoolSize)
	assert.Equal(t, 1, cfg.ASR.NumThreads)
	assert.Equal(t, 1, cfg.TTS.NumThreads)
	assert.Equal(t, 1, cfg.VAD.NumThreads)

	assert.Equal(t, "kokoro", cfg.TTS.Model)
	assert.Equal(t, 49, cfg.TTS.SpeakerID)
	assert.Equal(t, float32(1.0), cfg.TTS.Speed)
	assert.Equal(t, 1, cfg.TTS.PoolSize)

	assert.Equal(t, "openai_api", cfg.LLM.Provider)
	assert.Equal(t, "http://127.0.0.1:11434/v1", cfg.LLM.BaseURL)
	assert.Equal(t, "qwen3:0.6b", cfg.LLM.Model)
	assert.True(t, cfg.LLM.Stream)
	assert.Equal(t, []string{"web_search"}, cfg.LLM.Tools)
	assert.Empty(t, cfg.LLM.Thinking)
}

// TestLoadYAMLFile verifies values from an explicit YAML file override defaults.
func TestLoadYAMLFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
server:
  addr: ":9000"
  max_conns: 400
vad:
  model: ten
  pool_size: 8
asr:
  model: whisper
  pool_size: 4
  num_threads: 4
tts:
  speaker_id: 47
  speed: 1.2
  pool_size: 4
  num_threads: 8
llm:
  provider: ollama_api
  model: qwen2.5:7b
  stream: false
  thinking: low
  tools: []
`
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, ":9000", cfg.Server.Addr)
	assert.Equal(t, 400, cfg.Server.MaxConns)
	assert.Equal(t, "ten", cfg.VAD.Model)
	assert.Equal(t, 8, cfg.VAD.PoolSize)
	assert.Equal(t, "whisper", cfg.ASR.Model)
	assert.Equal(t, 4, cfg.ASR.PoolSize)
	assert.Equal(t, 4, cfg.ASR.NumThreads)
	assert.Equal(t, 47, cfg.TTS.SpeakerID)
	assert.Equal(t, float32(1.2), cfg.TTS.Speed)
	assert.Equal(t, 8, cfg.TTS.NumThreads)
	assert.Equal(t, "ollama_api", cfg.LLM.Provider)
	assert.Equal(t, "qwen2.5:7b", cfg.LLM.Model)
	assert.False(t, cfg.LLM.Stream)
	assert.Equal(t, "low", cfg.LLM.Thinking)
	assert.Empty(t, cfg.LLM.Tools)
	// untouched keys keep defaults
	assert.True(t, cfg.Server.RateLimitEnabled)
	assert.Equal(t, "http://127.0.0.1:11434/v1", cfg.LLM.BaseURL)
}

// TestLoadConfigDiscovery verifies config.yaml is discovered from the working directory.
func TestLoadConfigDiscovery(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("asr:\n  pool_size: 7\n"), 0o644))
	t.Chdir(dir)

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, 7, cfg.ASR.PoolSize)
}

// TestLoadEnvOverride verifies ACHATBOT_* environment variables override defaults and file values.
func TestLoadEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("llm:\n  model: from-file\n"), 0o644))

	t.Setenv("ACHATBOT_LLM_MODEL", "from-env")
	t.Setenv("ACHATBOT_ASR_POOL_SIZE", "16")
	t.Setenv("ACHATBOT_TTS_SPEED", "1.5")
	t.Setenv("ACHATBOT_LLM_STREAM", "false")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "from-env", cfg.LLM.Model)
	assert.Equal(t, 16, cfg.ASR.PoolSize)
	assert.Equal(t, float32(1.5), cfg.TTS.Speed)
	assert.False(t, cfg.LLM.Stream)
}

// TestLoadExplicitPathMissing verifies a missing explicit config file is an error,
// while a missing discovered file is not.
func TestLoadExplicitPathMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	require.Error(t, err)
}

// TestValidation verifies invalid values are rejected with the offending field named.
func TestValidation(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{"bad vad model", "vad:\n  model: webrtc\n", "vad.model"},
		{"bad asr model", "asr:\n  model: sensevoice-typo\n", "asr.model"},
		{"bad tts model", "tts:\n  model: piper\n", "tts.model"},
		{"bad llm provider", "llm:\n  provider: anthropic\n", "llm.provider"},
		{"bad thinking", "llm:\n  thinking: max\n", "llm.thinking"},
		{"thinking with openai_api", "llm:\n  provider: openai_api\n  thinking: high\n", "llm.thinking"},
		{"zero vad pool", "vad:\n  pool_size: 0\n", "vad.pool_size"},
		{"negative asr pool", "asr:\n  pool_size: -1\n", "asr.pool_size"},
		{"zero tts speed", "tts:\n  speed: 0\n", "tts.speed"},
		{"zero tts threads", "tts:\n  num_threads: 0\n", "tts.num_threads"},
		{"negative speaker", "tts:\n  speaker_id: -2\n", "tts.speaker_id"},
		{"empty llm model", "llm:\n  model: \"\"\n", "llm.model"},
		{"empty base url for openai", "llm:\n  base_url: \"\"\n", "llm.base_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0o644))
			_, err := Load(path)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidConfig)
			assert.True(t, strings.Contains(err.Error(), tc.wantSub), "error %q should mention %q", err, tc.wantSub)
		})
	}
}

// TestBaseURLNotRequiredForOllama verifies base_url may be empty when the
// native ollama_api provider is selected (it uses OLLAMA_HOST, not base_url).
func TestBaseURLNotRequiredForOllama(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("llm:\n  provider: ollama_api\n  base_url: \"\"\n"), 0o644))
	_, err := Load(path)
	require.NoError(t, err)
}
