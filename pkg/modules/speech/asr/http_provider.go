package asr

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/weedge/pipeline-go/pkg/logger"
)

// httpASRRate is the sample rate the GPU ASR service expects (Parakeet: 16 kHz).
const httpASRRate = 16000

// HTTPASRProvider transcribes speech via a remote GPU ASR service (Parakeet
// on NeMo) that accepts raw little-endian PCM16 mono at 16 kHz and returns
// {"text": "..."}. Instances are cheap (an HTTP client), so the pool can be
// large; the GPU service handles the concurrency and batching.
type HTTPASRProvider struct {
	baseURL string
	client  *http.Client
	name    string
}

// Transport, when set, is used for every HTTP request this package makes.
//
// The hook exists so the application can time downstream latency (capacity
// telemetry, tracing) without this package importing the code that consumes
// the measurements — a speech module has no business depending on the
// platform contract. nil means net/http's DefaultTransport, so leaving it
// unset changes nothing.
var Transport http.RoundTripper

// NewHTTPASRProvider builds a provider pointing at baseURL (e.g.
// http://127.0.0.1:8890). Returns nil if the service is unreachable at startup.
func NewHTTPASRProvider(baseURL string) *HTTPASRProvider {
	p := &HTTPASRProvider{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 15 * time.Second, Transport: Transport},
		name:    "parakeetHTTP",
	}
	resp, err := p.client.Get(baseURL + "/health")
	if err != nil {
		logger.Error("HTTP ASR health check failed", "url", baseURL, "err", err)
		return nil
	}
	resp.Body.Close()
	return p
}

// Transcribe posts the utterance PCM16 to the GPU service and returns the text.
func (p *HTTPASRProvider) Transcribe(audio []byte) string {
	if len(audio) < 2 {
		return ""
	}
	resp, err := p.client.Post(p.baseURL+"/asr", "application/octet-stream", bytes.NewReader(audio))
	if err != nil {
		logger.Error("HTTP ASR request failed", "err", err)
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		logger.Error("HTTP ASR decode failed", "err", err, "body", string(body))
		return ""
	}
	return r.Text
}

func (p *HTTPASRProvider) Warmup()        {}
func (p *HTTPASRProvider) Reset() error   { return nil }
func (p *HTTPASRProvider) Release() error { return nil }
func (p *HTTPASRProvider) Name() string   { return p.name }
