package rexa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// RetrySchedule is the backoff the platform's spec §5.3 asks callers to use
// when a callback POST fails.
//
// Retrying at all is what matters; the exact intervals are matched only
// because agreeing with a documented contract is free. Retries are safe
// without any bookkeeping on our side: the platform dedupes on
// (type, session_id) and answers {"ok":true,"duplicate":true} for a repeat.
//
// There is deliberately no durable/on-disk queue behind this. If the agent
// process dies the report is lost regardless, and the platform's
// reconcileStuckInProgress cron already sweeps sessions whose report never
// arrived. What this protects against is the common case — a brief platform
// restart or network blip — where dropping the report would silently record a
// completed call as `failed` half an hour later, transcript and all.
var RetrySchedule = []time.Duration{
	1 * time.Second,
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	12 * time.Minute,
}

// SentimentRetrySchedule is the ladder for mid-call sentiment events.
//
// Short on purpose. A sentiment alert exists so a human can act while the
// caller is still on the line; a delivery that finally succeeds twelve minutes
// later has failed at the only thing it was for, and arrives as noise about a
// call that has long since ended. Two quick retries cover a blip, and then we
// stop.
var SentimentRetrySchedule = []time.Duration{
	1 * time.Second,
	3 * time.Second,
}

// callbackTimeout bounds a single POST attempt. The platform's ingress is
// verify + insert + enqueue with a sub-50ms p95 target, so anything near this
// bound means it is unhealthy and we are better off retrying than waiting.
const callbackTimeout = 10 * time.Second

// Poster sends HMAC-signed callbacks to the platform.
//
// Safe for concurrent use: it holds no per-call state.
type Poster struct {
	secret string
	http   *http.Client
	// sleep is injectable so tests exercise the retry ladder without
	// actually waiting 12 minutes.
	sleep func(context.Context, time.Duration) error
}

// NewPoster builds a Poster signing with the inbound secret — the one the
// platform calls AGENT_HMAC_SECRET_INBOUND, used for agent → platform traffic.
// Signing callbacks with the outbound secret is a silent 401 on their side.
func NewPoster(secret string) *Poster {
	return &Poster{
		secret: secret,
		http:   &http.Client{Timeout: callbackTimeout},
		sleep:  sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Post delivers payload to url, retrying transient failures.
//
// Blocks for as long as the retry ladder runs (up to ~15 min), so callers that
// must not stall should run it in a goroutine. It returns nil as soon as the
// platform accepts the callback, and the last error if every attempt failed.
func (p *Poster) Post(ctx context.Context, url string, payload any) error {
	return p.postWithSchedule(ctx, url, payload, RetrySchedule)
}

// postWithSchedule is Post with an explicit retry ladder, so an event whose
// value expires can give up early instead of retrying into irrelevance.
func (p *Poster) postWithSchedule(ctx context.Context, url string, payload any, schedule []time.Duration) error {
	body, err := json.Marshal(payload)
	if err != nil {
		// Unmarshalable payload is a bug in our own struct tags; retrying
		// cannot fix it.
		return fmt.Errorf("marshal callback: %w", err)
	}

	// One nonce for the whole ladder, matching what the platform does on its
	// dispatch retries: every attempt is the same logical event, so the
	// receiver can collapse them rather than seeing N distinct deliveries.
	nonce := uuid.NewString()

	var lastErr error
	for attempt := 0; ; attempt++ {
		err := p.postOnce(ctx, url, body, nonce)
		if err == nil {
			return nil
		}
		lastErr = err

		// A permanent rejection is a bug in the payload or the secret. The
		// spec is explicit: do not retry 4xx — fix it and resend.
		var perm permanentError
		if errors.As(err, &perm) {
			return err
		}
		if attempt >= len(schedule) {
			break
		}
		delay := schedule[attempt]
		log.Printf("rexa: callback to %s failed (attempt %d/%d): %v — retrying in %s",
			url, attempt+1, len(schedule)+1, err, delay)
		if serr := p.sleep(ctx, delay); serr != nil {
			return fmt.Errorf("callback abandoned: %w (last error: %v)", serr, lastErr)
		}
	}
	return fmt.Errorf("callback to %s failed after %d attempts: %w", url, len(schedule)+1, lastErr)
}

// permanentError marks a failure that retrying cannot fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func (p *Poster) postOnce(ctx context.Context, url string, body []byte, nonce string) error {
	env := SignWithNonce(p.secret, body, nonce)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return permanentError{fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set(HeaderSignature, env.Signature)
	req.Header.Set(HeaderTimestamp, env.Timestamp)
	req.Header.Set(HeaderNonce, env.Nonce)

	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err) // network — retryable
	}
	defer resp.Body.Close()
	// Drain before close so the connection returns to the keep-alive pool
	// rather than being torn down after every callback.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 500:
		return fmt.Errorf("platform %d: %s", resp.StatusCode, snippet)
	default:
		// 4xx. 401 means our secret or canonical string is wrong; 400 means the
		// body shape is wrong. Both need a code change, not another attempt.
		return permanentError{fmt.Errorf("platform rejected %d: %s", resp.StatusCode, snippet)}
	}
}

// PostEndOfCall delivers an end-of-call report.
//
// Must be called for every dispatched call, including those that never ran a
// pipeline (voicemail, no-answer, busy) — the platform marks any session it
// never hears about as `failed` after 30 minutes.
func (p *Poster) PostEndOfCall(ctx context.Context, url string, r EndOfCallReport) error {
	return p.Post(ctx, url, r)
}

// PostTransferInitiated delivers a mid-call transfer notification.
func (p *Poster) PostTransferInitiated(ctx context.Context, url string, e TransferInitiatedEvent) error {
	return p.Post(ctx, url, e)
}

// PostRecordingSaved delivers a finalised-recording notification.
//
// Goes to the same webhook_url as the end-of-call report and arrives after it,
// because the carrier finalises a recording tens of seconds after the call
// ends. The full retry ladder applies: a recording link the platform never
// receives is a recording the tenant cannot play.
func (p *Poster) PostRecordingSaved(ctx context.Context, url string, e RecordingSavedEvent) error {
	return p.Post(ctx, url, e)
}

// PostSentiment delivers a mid-call sentiment change to the dispatch's
// sentiment_webhook — a DIFFERENT url from the end-of-call report.
//
// Uses a short-ladder poster: the platform's whole reason for this event is to
// alert a human while the caller is still on the line, so a delivery that
// succeeds twelve minutes later has failed at the only thing it was for.
func (p *Poster) PostSentiment(ctx context.Context, url string, e SentimentEvent) error {
	return p.postWithSchedule(ctx, url, e, SentimentRetrySchedule)
}
