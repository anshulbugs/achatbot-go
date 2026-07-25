package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"achatbot/pkg/consts"
	"achatbot/pkg/telnyx"
)

// loadConn is an in-process IWebSocketConn used by the synthetic load test. A
// producer feeds pre-rendered caller media frames into `in`; outbound bot audio
// is discarded (latency is measured by the Telnyx serializer's hook instead).
type loadConn struct {
	in     chan []byte
	closed chan struct{}
	once   sync.Once
}

func newLoadConn() *loadConn {
	return &loadConn{in: make(chan []byte, 128), closed: make(chan struct{})}
}

func (c *loadConn) ReadMessage() (consts.MessageType, []byte, error) {
	select {
	case msg, ok := <-c.in:
		if !ok {
			return consts.BinaryMessage, nil, io.EOF
		}
		return consts.BinaryMessage, msg, nil
	case <-c.closed:
		return consts.BinaryMessage, nil, io.EOF
	}
}

func (c *loadConn) WriteMessage(_ consts.MessageType, _ []byte) error { return nil }

func (c *loadConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// mediaMsg wraps a base64 µ-law payload in a Telnyx inbound-media envelope.
func mediaMsg(payloadB64 string) []byte {
	b, _ := json.Marshal(map[string]any{
		"event": "media",
		"media": map[string]string{"payload": payloadB64},
	})
	return b
}

// muLawSilenceFrame is one 20 ms µ-law silence frame (160 samples; µ-law 0 = 0xFF).
var silenceB64 = func() string {
	s := make([]byte, 160)
	for i := range s {
		s[i] = 0xFF
	}
	return base64.StdEncoding.EncodeToString(s)
}()

// synthCallerFrames renders text via the GPU TTS service and returns it as 20 ms
// base64 µ-law/8 kHz frames — the exact wire format Telnyx forwards for caller
// audio, so it drives the same VAD->ASR->LLM->TTS path a real call does.
func synthCallerFrames(ttsURL, voice, text string) ([]string, error) {
	body, _ := json.Marshal(map[string]any{"input": text, "voice": voice, "speed": 1.0})
	resp, err := http.Post(ttsURL+"/tts", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	pcm24, _ := io.ReadAll(resp.Body)
	pcm8 := telnyx.ResamplePCM16(pcm24, 24000, 8000) // Telnyx µ-law is 8 kHz
	mulaw := telnyx.PCM16ToMuLaw(pcm8)
	var out []string
	for i := 0; i < len(mulaw); i += 160 {
		end := i + 160
		if end > len(mulaw) {
			end = len(mulaw)
		}
		out = append(out, base64.StdEncoding.EncodeToString(mulaw[i:end]))
	}
	return out, nil
}

// loadStats collects per-turn wire latencies across all sessions.
type loadStats struct {
	mu      sync.Mutex
	lat     []time.Duration
	turns   int
	started int
}

func (s *loadStats) add(d time.Duration) {
	s.mu.Lock()
	s.lat = append(s.lat, d)
	s.turns++
	s.mu.Unlock()
}

func pct(sorted []time.Duration, p float64) int {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p / 100 * float64(len(sorted)-1))
	return int(sorted[i].Milliseconds())
}

func (s *loadStats) summary(concurrency int, dur time.Duration) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := append([]time.Duration(nil), s.lat...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	var sum time.Duration
	for _, d := range cp {
		sum += d
	}
	avg := 0
	if len(cp) > 0 {
		avg = int((sum / time.Duration(len(cp))).Milliseconds())
	}
	return map[string]any{
		"concurrency": concurrency,
		"duration_s":  int(dur.Seconds()),
		"turns":       len(cp),
		"turns_per_s": fmt.Sprintf("%.1f", float64(len(cp))/dur.Seconds()),
		"latency_ms":  map[string]int{"avg": avg, "p50": pct(cp, 50), "p90": pct(cp, 90), "p99": pct(cp, 99), "max": pct(cp, 100)},
	}
}

// runLoadSession drives one synthetic caller through the real pipeline for the
// test window: repeated turns of (lead silence -> speech -> trailing silence to
// endpoint and let the bot reply). The serializer's latency hook records each
// turn's wire latency into stats.
func runLoadSession(idx int, clips [][]string, deadline time.Time, stats *loadStats) {
	conn := newLoadConn()
	ser := telnyx.NewSerializer(consts.DefaultRate)
	ser.SetLatencyHook(func(d time.Duration) { stats.add(d) })

	stop := make(chan struct{})
	sc := sessionConfig{
		clientID:           fmt.Sprintf("load_%d", idx),
		systemPrompt:       cfg.Server.SystemPrompt,
		voiceID:            cfg.TTS.SpeakerID,
		speed:              cfg.TTS.Speed,
		llmModel:           cfg.LLM.Model,
		addWavHeader:       false,
		hello:              "",
		allowInterruptions: true,
		audioOutFrameMS:    40,
		stop:               stop,
	}
	done := make(chan struct{})
	go func() { runVoiceSession(conn, ser, sc); close(done) }()

	silence := mediaMsg(silenceB64)
	send := func(msg []byte) bool {
		select {
		case conn.in <- msg:
		case <-time.After(500 * time.Millisecond):
			return false
		}
		time.Sleep(20 * time.Millisecond) // real-time pacing (20 ms/frame)
		return true
	}
	sendSilence := func(n int) {
		for i := 0; i < n && time.Now().Before(deadline); i++ {
			send(silence)
		}
	}

	turn := 0
	for time.Now().Before(deadline) {
		sendSilence(10) // 200 ms lead
		clip := clips[(idx+turn)%len(clips)]
		for _, f := range clip {
			if time.Now().After(deadline) {
				break
			}
			send(mediaMsg(f))
		}
		sendSilence(190) // ~3.8 s: endpoint + let the bot reply before next turn
		turn++
	}
	close(stop)  // cancel the pipeline task so it returns and releases pool instances
	conn.Close() // unblock the input read loop
	select {
	case <-done:
	case <-time.After(20 * time.Second):
	}
}

// handleLoadTest runs a synthetic concurrency test: POST/GET /api/loadtest?n=100&secs=60.
// Requires vad/asr/tts pool_size >= n or sessions block on pool.Get.
func handleLoadTest(w http.ResponseWriter, r *http.Request) {
	writeCORS(w)
	if r.Method == http.MethodOptions {
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 {
		n = 20
	}
	secs, _ := strconv.Atoi(r.URL.Query().Get("secs"))
	if secs <= 0 {
		secs = 60
	}

	texts := []string{
		"Hi, I have five years of experience with Python, React, and Kubernetes. What does the interview process look like?",
		"Can you tell me about the role and what a typical day on the team looks like?",
		"I have also worked with SQL, Node, and a bit of Java. Is this a good fit for the position?",
	}
	var clips [][]string
	for _, t := range texts {
		f, err := synthCallerFrames(cfg.TTS.HTTPURL, "am_michael", t)
		if err != nil {
			http.Error(w, "failed to build caller clip: "+err.Error(), http.StatusInternalServerError)
			return
		}
		clips = append(clips, f)
	}

	stats := &loadStats{}
	start := time.Now()
	deadline := start.Add(time.Duration(secs) * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); runLoadSession(i, clips, deadline, stats) }(i)
		time.Sleep(25 * time.Millisecond) // stagger ramp to avoid a pool.Get thundering herd
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats.summary(n, time.Since(start)))
}
