package main

// Platform call-agent contract wiring.
//
// Implements rexa.Dispatcher on top of the existing Telnyx call machinery, and
// registers the contract endpoints when the platform secrets are configured.
//
// Nothing here runs unless REXA_OUTBOUND_HMAC_SECRET and
// REXA_INBOUND_HMAC_SECRET are both set. With them unset the process behaves
// exactly as it did before: the demo endpoints are registered, the contract
// ones are not, and no call ever carries a `rexa` block.

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"achatbot/pkg/modules/speech/asr"
	"achatbot/pkg/modules/speech/tts"
	"achatbot/pkg/rexa"
	"achatbot/pkg/telnyx"
)

// rexaPoster signs and delivers callbacks to the platform. nil when the
// contract is not configured, which is what makes reportCallEnded a no-op on
// a demo-only deployment.
var rexaPoster *rexa.Poster

// rexaVoices maps the platform's voice vocabulary onto kokoro speaker ids.
var rexaVoices *rexa.VoiceResolver

// rexaMetrics is the live capacity + bottleneck registry behind /health,
// /dashboard and the at_capacity backpressure. nil when the contract is not
// configured, so every instrumentation point must nil-check — the demo path
// runs with it unset.
var rexaMetrics *rexa.Metrics

// Capacity transitions, all no-ops when the contract is not configured.
//
// A call is reserved at dispatch (admission), promoted to on_gpu when its
// pipeline starts, reclassified to voicemail if a machine answered, and
// released on hangup. Only the reserved and on_gpu states cost GPU capacity.

// markOnGPU promotes a call to a live pipeline. Keyed by carrier call id.
func markOnGPU(callID string) {
	if rexaMetrics != nil {
		rexaMetrics.MarkOnGPU(callID)
	}
}

// markVoicemail releases a call's GPU capacity: with AMD enabled the pipeline
// never started, so it holds no pool slots and only the announcement remains.
func markVoicemail(callID string) {
	if rexaMetrics != nil {
		rexaMetrics.MarkVoicemail(callID)
	}
}

// releaseCall ends a call and records what it became for the answer-rate
// estimate. Safe for untracked ids, so the demo path can call it freely.
func releaseCall(callID string) {
	if rexaMetrics != nil {
		rexaMetrics.Release(callID)
	}
}

// markAnswered records dial-to-answer time for the ring-time estimate.
func markAnswered(callID string) {
	if rexaMetrics != nil {
		rexaMetrics.MarkAnswered(callID)
	}
}

// rexaConfigured reports whether both platform secrets are present. Both are
// required together: with only one we could either verify dispatches but never
// report, or report but accept unauthenticated calls. An agent that accepts
// calls it cannot report on is worse than one that does not start.
func rexaConfigured() bool {
	return os.Getenv("REXA_OUTBOUND_HMAC_SECRET") != "" && os.Getenv("REXA_INBOUND_HMAC_SECRET") != ""
}

// initRexaTelemetry creates the metrics registry and wraps the speech
// providers' HTTP transports so every downstream request is timed.
//
// MUST be called before the provider pools are built. Each provider captures
// the package-level Transport when it constructs its http.Client, so setting
// it afterwards silently does nothing and every tier reports `unknown`
// forever — a failure that looks like "the dashboard is broken" rather than
// "the wiring ran in the wrong order".
func initRexaTelemetry() {
	if !rexaConfigured() {
		return
	}
	rexaMetrics = rexa.NewMetrics(cfg.Server.MaxGPUCalls)
	rexaMetrics.SetMaxTotalCalls(cfg.Server.MaxTotalCalls)
	// Already validated at config load, so an error here is impossible; fall
	// back to the safe full weight rather than panicking if that ever changes.
	weight, err := cfg.Server.ResolveHumanAnswerWeight()
	if err != nil {
		log.Printf("rexa: %v — using full weight", err)
		weight = 1.0
	}
	rexaMetrics.SetHumanWeight(weight)

	// Reap leaked reservations. Without this a lost hangup webhook removes a
	// slot permanently, and enough of them silently stop the agent accepting
	// work while it still reports itself healthy — strictly worse than the
	// dispatch-vs-pipeline lag reservations were introduced to fix.
	go func() {
		for range time.Tick(15 * time.Second) {
			if n := rexaMetrics.ReapStale(); n > 0 {
				log.Printf("rexa: reaped %d stale reservation(s) — hangup events are being missed", n)
			}
		}
	}()
	// Timing the transport measures true wire latency — what the caller
	// actually waits for — rather than inferring load from GPU utilisation,
	// and the providers never learn that metrics exist.
	asr.Transport = rexaMetrics.Tripper(rexa.TierASR, nil)
	tts.Transport = rexaMetrics.Tripper(rexa.TierTTS, nil)
}

// platformDispatcher places calls on behalf of the platform.
type platformDispatcher struct {
	// publicURL is OUR externally reachable base URL, used to build the
	// per-call webhook and media-stream URLs we hand to Telnyx. Distinct from
	// the platform's webhook_url, which is where we post reports.
	publicURL string
}

// DispatchPhone places an outbound call using the dispatch's own credentials.
//
// Returns as soon as the carrier accepts the dial. The platform abandons a
// dispatch after 30 s and retries, so this must not wait for the conversation.
func (d *platformDispatcher) DispatchPhone(ctx context.Context, req rexa.PhoneDispatchRequest) (rexa.DispatchResponse, error) {
	creds, err := req.TelecomCredentials.Telnyx()
	if err != nil {
		// Permanent: the platform fails the session immediately on this code
		// rather than retrying a dispatch that can never succeed.
		return rexa.DispatchResponse{}, rexa.Errorf(rexa.ErrCodeProviderCredsInvalid, "%v", err)
	}
	if d.publicURL == "" {
		return rexa.DispatchResponse{}, rexa.Errorf(rexa.ErrCodeInternal,
			"agent has no public URL configured; cannot receive carrier webhooks")
	}

	client := telnyx.NewClient(creds.APIKey, creds.ConnectionID, req.FromNumber, d.publicURL)
	voiceID, matched := rexaVoices.Resolve(req.Voice)
	if !matched {
		log.Printf("rexa: session=%s voice %q unmapped, using speaker %d",
			req.SessionID, req.Voice, voiceID)
	}

	p := &callParams{
		To:               req.ToNumber,
		Hello:            req.HelloMessage,
		SystemPrompt:     req.SystemPrompt,
		VoicemailMessage: req.VoicemailMessage,
		VoiceID:          voiceID,
		Speed:            cfg.TTS.Speed,
		Volume:           cfg.TTS.Gain,
		LLMModel:         cfg.LLM.Model,
		amdCh:            make(chan string, 2),
		beepCh:           make(chan string, 2),
		platform: &rexaCall{
			sessionID:  req.SessionID,
			tenantID:   req.TenantID,
			webhookURL: req.WebhookURL,
			direction:  "outbound",
			client:     client,
			// Anchored provisionally at dispatch; markAnswered resets it to
			// the pickup time so turn timings start from the conversation.
			transcript: rexa.NewTranscript(time.Now()),
		},
	}

	// Pre-render the greeting and voicemail message before the line rings, so
	// neither costs a TTS slot once the call is live and a machine-answered
	// call never waits on one.
	prerenderAnnouncements(p)

	webhookURL := d.publicURL + "/telnyx/webhook"
	callControlID, err := client.Dial(ctx, req.ToNumber, webhookURL, "", amdModeFor(p))
	if err != nil {
		log.Printf("rexa: session=%s dial failed: %v", req.SessionID, err)
		// The call never rang, so nothing downstream will ever emit a report
		// for it. Report the failure here or the platform waits 30 minutes and
		// marks the session failed with no detail.
		// The slot was reserved at admission; the call never rang, so give it
		// back rather than waiting for the reaper.
		releaseCall(req.SessionID)
		d.reportDispatchFailure(req.SessionID, req.TenantID, req.WebhookURL, "outbound")
		return rexa.DispatchResponse{}, rexa.Errorf(rexa.ErrCodeProviderUnavailable, "dial failed: %v", err)
	}

	// Admission reserved under session_id because the carrier had not yet
	// named the call. Every later transition arrives from a webhook that knows
	// only the call-control id.
	if rexaMetrics != nil {
		rexaMetrics.Rekey(req.SessionID, callControlID)
	}
	calls.put(callControlID, p)
	log.Printf("rexa: session=%s dialing %s call=%s", req.SessionID, req.ToNumber, callControlID)
	return rexa.DispatchResponse{Status: "accepted", AgentSessionID: callControlID}, nil
}

// DispatchIncoming answers a carrier leg that is already ringing.
//
// The platform has already accepted the call at its edge and hands us the
// carrier's call-control id, so there is no dial — we answer and let the
// existing webhook flow drive the rest.
func (d *platformDispatcher) DispatchIncoming(ctx context.Context, req rexa.IncomingDispatchRequest) (rexa.DispatchResponse, error) {
	creds, err := req.TelecomCredentials.Telnyx()
	if err != nil {
		return rexa.DispatchResponse{}, rexa.Errorf(rexa.ErrCodeProviderCredsInvalid, "%v", err)
	}
	client := telnyx.NewClient(creds.APIKey, creds.ConnectionID, req.ToNumber, d.publicURL)
	voiceID, _ := rexaVoices.Resolve(req.Voice)

	hello := req.HelloMessage
	if hello == "" {
		hello = "Hello! Thanks for calling. How can I help you today?"
	}
	p := &callParams{
		To:           req.FromNumber,
		Hello:        hello,
		SystemPrompt: req.SystemPrompt,
		VoiceID:      voiceID,
		Speed:        cfg.TTS.Speed,
		Volume:       cfg.TTS.Gain,
		LLMModel:     cfg.LLM.Model,
		amdCh:        make(chan string, 2),
		beepCh:       make(chan string, 2),
		platform: &rexaCall{
			sessionID:  req.SessionID,
			tenantID:   req.TenantID,
			webhookURL: req.WebhookURL,
			direction:  "inbound",
			client:     client,
			transcript: rexa.NewTranscript(time.Now()),
		},
	}
	// Register BEFORE answering: the carrier can deliver call.answered before
	// Answer() returns, and an unregistered call is dropped on the floor.
	calls.put(req.CCID, p)
	// Counted, never refused. The leg is already ringing with a human on it,
	// so refusing costs a real answered call — but counting it is what makes
	// inbound load reduce the outbound allowance instead of silently
	// exceeding the ceiling.
	if rexaMetrics != nil {
		rexaMetrics.Track(req.CCID)
	}

	if err := client.Answer(ctx, req.CCID); err != nil {
		calls.del(req.CCID)
		releaseCall(req.CCID)
		log.Printf("rexa: session=%s answer failed: %v", req.SessionID, err)
		d.reportDispatchFailure(req.SessionID, req.TenantID, req.WebhookURL, "inbound")
		return rexa.DispatchResponse{}, rexa.Errorf(rexa.ErrCodeProviderUnavailable, "answer failed: %v", err)
	}
	log.Printf("rexa: session=%s answered inbound call=%s", req.SessionID, req.CCID)
	return rexa.DispatchResponse{Status: "accepted", AgentSessionID: req.CCID}, nil
}

// DispatchWebrtc is not implemented yet.
//
// The chosen design is a Daily room joined over SIP, with Telnyx bridging the
// leg into the existing media path. Until that lands this fails loudly rather
// than returning a room that nobody is in — a browser joining an empty room
// hears silence and looks exactly like a broken agent.
func (d *platformDispatcher) DispatchWebrtc(context.Context, rexa.WebrtcDispatchRequest) (rexa.WebrtcDispatchResponse, error) {
	return rexa.WebrtcDispatchResponse{}, rexa.Errorf(rexa.ErrCodeProviderUnavailable,
		"WebRTC rooms are not yet available on this agent")
}

// reportDispatchFailure tells the platform a call never got off the ground.
//
// Without this the session sits in_progress until the platform's reconciler
// marks it failed half an hour later, with no cause recorded.
func (d *platformDispatcher) reportDispatchFailure(sessionID, tenantID, webhookURL, direction string) {
	if rexaPoster == nil {
		return
	}
	status, reason := rexa.Outcome{DispatchFailed: true, Direction: direction}.Report()
	report := rexa.EndOfCallReport{
		SessionID:  sessionID,
		TenantID:   tenantID,
		CallStatus: status,
		EndReason:  reason,
		EndedAt:    rexa.ISOTime(time.Now()),
	}
	go func() {
		if err := rexaPoster.PostEndOfCall(context.Background(), webhookURL, report); err != nil {
			log.Printf("rexa: dispatch-failure report FAILED for session=%s: %v", sessionID, err)
		}
	}()
}

// registerRexaRoutes wires the contract endpoints onto mux when the platform
// secrets are present, and reports whether it did.
//
// Both secrets are required together: with only one we could either verify
// dispatches but never report, or report but accept unauthenticated calls.
// Neither half is useful, and a half-configured agent that accepts calls it
// cannot report on is worse than one that does not start.
func registerRexaRoutes(mux *http.ServeMux) bool {
	outbound := os.Getenv("REXA_OUTBOUND_HMAC_SECRET")
	inbound := os.Getenv("REXA_INBOUND_HMAC_SECRET")
	if outbound == "" || inbound == "" {
		if outbound != "" || inbound != "" {
			log.Printf("rexa: DISABLED — only one of REXA_OUTBOUND_HMAC_SECRET / " +
				"REXA_INBOUND_HMAC_SECRET is set; both are required")
		}
		return false
	}

	publicURL := os.Getenv("TELNYX_PUBLIC_URL")
	if publicURL == "" && telnyxClient != nil {
		publicURL = telnyxClient.PublicURL()
	}
	if publicURL == "" {
		log.Printf("rexa: DISABLED — TELNYX_PUBLIC_URL is unset, so carrier " +
			"webhooks and media streams have nowhere to arrive")
		return false
	}

	rexaPoster = rexa.NewPoster(inbound)
	rexaVoices = rexa.NewVoiceResolver(kokoroVoiceCatalog(), parseVoiceOverrides(), cfg.TTS.SpeakerID)
	if rexaMetrics == nil {
		// initRexaTelemetry must have run first; without it the transports are
		// unwrapped and every tier would report `unknown` forever.
		log.Printf("rexa: DISABLED — telemetry was not initialised before route registration")
		return false
	}

	srv := rexa.NewServer(outbound, rexa.NewMemoryNonceStore(),
		&platformDispatcher{publicURL: strings.TrimRight(publicURL, "/")}, rexaMetrics)
	srv.Routes(mux)
	srv.RoutesDashboard(mux)

	ceiling := "unlimited"
	if cfg.Server.MaxGPUCalls > 0 {
		ceiling = strconv.Itoa(cfg.Server.MaxGPUCalls)
	}
	log.Printf("rexa: contract endpoints enabled (/health /connection /incoming /connection_webrtc), "+
		"dashboard at /dashboard, gpu-call ceiling=%s, public=%s", ceiling, publicURL)
	return true
}

// kokoroVoiceCatalog exposes the local voice list to the resolver as
// name → speaker id.
func kokoroVoiceCatalog() map[string]int {
	out := make(map[string]int, len(kokoroVoices))
	for _, v := range kokoroVoices {
		out[v.Name] = v.ID
	}
	return out
}

// parseVoiceOverrides reads REXA_VOICE_MAP, a comma-separated list of
// platform-voice=speaker-id pairs, e.g. "leah=3,marcus=16".
//
// This is the seam for reconciling the platform's voice catalogue with ours
// without a code change or a redeploy of the platform.
func parseVoiceOverrides() map[string]int {
	raw := os.Getenv("REXA_VOICE_MAP")
	if raw == "" {
		return nil
	}
	out := map[string]int{}
	for _, pair := range strings.Split(raw, ",") {
		name, idStr, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			log.Printf("rexa: ignoring malformed REXA_VOICE_MAP entry %q (want name=id)", pair)
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(idStr))
		if err != nil {
			log.Printf("rexa: ignoring REXA_VOICE_MAP entry %q: %v", pair, err)
			continue
		}
		out[strings.TrimSpace(name)] = id
	}
	return out
}
