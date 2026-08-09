package rexa

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Live-call state published to the tenant's own Redis, for anything that needs
// to watch calls as they happen rather than after they end.
//
// WHY REDIS AS WELL AS WEBHOOKS. The webhooks are a durable record: signed,
// retried for twelve minutes, and worth reading after the fact. That machinery
// is exactly wrong for a wallboard, which wants the current state of forty
// calls right now and does not care what happened at 09:14. Redis answers that
// in one read.
//
// So the two carry different things and neither replaces the other. A webhook
// is an EVENT the platform must not miss. This is STATE, and it is disposable —
// if the agent restarts, the keys expire and the world moves on.
//
// SHAPE OF THE CONTRACT. Two things per call, and a set to find them by:
//
//	rexa:call:{session_id}        HASH   current state, TTL 1h
//	rexa:call:{session_id}:events CHANNEL  every change, pub/sub
//	rexa:calls:live               SET    session ids currently live
//
// A reader that wants "everything happening now" reads the set and then the
// hashes. A reader that wants to react as it happens subscribes to the channel
// pattern `rexa:call:*:events`. Neither needs to poll the agent.
//
// FAILURE IS ALWAYS SILENT HERE. A tenant's Redis being down, slow, or wrongly
// configured must never affect a call in progress — it is a telemetry sink, not
// a dependency. Every publish is fire-and-forget with a short timeout, and the
// error is logged once per call rather than per turn.

// LiveStatus values. The lifecycle a watcher sees, in order.
const (
	LiveStatusDialing     = "dialing"
	LiveStatusRinging     = "ringing"
	LiveStatusInProgress  = "in_progress"
	LiveStatusVoicemail   = "voicemail"
	LiveStatusTransferred = "transferred"
	LiveStatusEnded       = "ended"
)

// liveKeyTTL bounds how long a call's state survives without an update.
//
// Long enough to outlive any real call, short enough that a crashed agent does
// not leave a wallboard showing calls that ended yesterday. Refreshed on every
// update, so a long call never expires under a watcher.
const liveKeyTTL = time.Hour

// livePublishTimeout is the budget for one publish. Deliberately tight: this
// runs on the call's own goroutines, and a tenant's unreachable Redis must cost
// the caller nothing.
const livePublishTimeout = 500 * time.Millisecond

// LiveCallState is the value published — as a Redis hash for reading, and as
// JSON on the channel for watching.
type LiveCallState struct {
	SessionID string `json:"session_id"`
	TenantID  string `json:"tenant_id"`
	Status    string `json:"status"`
	// CCID is the carrier call id, for correlating with provider logs.
	CCID string `json:"CCID,omitempty"`
	// JoinURL is where an operator can join or listen to this call live. Empty
	// until the WebRTC path exists — a watcher must treat it as optional rather
	// than assume every live call has one.
	JoinURL string `json:"join_url,omitempty"`
	// Sentiment is the last mid-call classification, when sentiment analysis is
	// enabled for the call. Empty otherwise, which is not the same as neutral.
	Sentiment  string `json:"sentiment,omitempty"`
	ToNumber   string `json:"to_number,omitempty"`
	FromNumber string `json:"from_number,omitempty"`
	// StartedAt is when the call was dispatched, not when it was answered.
	StartedAt string `json:"started_at,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// LivePublisher writes call state to a tenant's Redis.
//
// One publisher per call, because the connection details arrive per dispatch:
// each tenant points at their own Redis, and there is no shared instance to
// pool. A nil publisher is valid and does nothing, which is what every call
// without Redis details gets.
type LivePublisher struct {
	rdb       *redis.Client
	state     LiveCallState
	failedLog bool
}

// RedisTarget is the connection detail carried on a dispatch.
type RedisTarget struct {
	Host     string
	Port     int
	DB       int
	Password string
}

// Configured reports whether there is anywhere to publish to.
func (t RedisTarget) Configured() bool { return t.Host != "" && t.Port > 0 }

// NewLivePublisher connects to the tenant's Redis. Returns nil when no target
// is configured, and nil is safe to call every method on.
//
// The dial happens lazily inside go-redis, so this never blocks a dispatch on a
// tenant's unreachable host — the first publish absorbs the failure instead,
// and publishes never block a call.
func NewLivePublisher(t RedisTarget, st LiveCallState) *LivePublisher {
	if !t.Configured() {
		return nil
	}
	return &LivePublisher{
		rdb: redis.NewClient(&redis.Options{
			Addr:         t.Host + ":" + strconv.Itoa(t.Port),
			DB:           t.DB,
			Password:     t.Password,
			DialTimeout:  livePublishTimeout,
			ReadTimeout:  livePublishTimeout,
			WriteTimeout: livePublishTimeout,
			// One connection per call: these are short-lived and low-volume, and
			// a bigger pool per call would multiply sockets by concurrency.
			PoolSize: 1,
		}),
		state: st,
	}
}

func liveKey(sessionID string) string     { return "rexa:call:" + sessionID }
func liveChannel(sessionID string) string { return "rexa:call:" + sessionID + ":events" }
func liveSetKey() string                  { return "rexa:calls:live" }

// Status publishes a lifecycle change.
func (p *LivePublisher) Status(status string) {
	if p == nil {
		return
	}
	p.state.Status = status
	p.publish()
}

// Sentiment publishes a mid-call sentiment change, keeping the current status.
func (p *LivePublisher) Sentiment(v string) {
	if p == nil {
		return
	}
	p.state.Sentiment = v
	p.publish()
}

// JoinURL records where an operator can join, and publishes.
func (p *LivePublisher) JoinURL(url string) {
	if p == nil {
		return
	}
	p.state.JoinURL = url
	p.publish()
}

// CCID records the carrier call id once the dial returns.
func (p *LivePublisher) CCID(ccid string) {
	if p == nil {
		return
	}
	p.state.CCID = ccid
}

// Close marks the call ended and releases the connection.
//
// The key is left behind with its TTL rather than deleted: a watcher that polls
// every few seconds would otherwise see live calls vanish with no final state,
// and "ended" is the most useful thing it can read.
func (p *LivePublisher) Close() {
	if p == nil {
		return
	}
	p.state.Status = LiveStatusEnded
	p.publish()
	ctx, cancel := context.WithTimeout(context.Background(), livePublishTimeout)
	p.rdb.SRem(ctx, liveSetKey(), p.state.SessionID)
	cancel()
	_ = p.rdb.Close()
}

func (p *LivePublisher) publish() {
	p.state.UpdatedAt = ISOTime(time.Now())
	blob, err := json.Marshal(p.state)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), livePublishTimeout)
	defer cancel()

	key := liveKey(p.state.SessionID)
	// One round trip for all of it. Individually these are four commands on a
	// path that runs on every turn of every call.
	pipe := p.rdb.Pipeline()
	pipe.HSet(ctx, key, p.state.asHash())
	pipe.Expire(ctx, key, liveKeyTTL)
	if p.state.Status == LiveStatusEnded {
		pipe.SRem(ctx, liveSetKey(), p.state.SessionID)
	} else {
		pipe.SAdd(ctx, liveSetKey(), p.state.SessionID)
	}
	pipe.Publish(ctx, liveChannel(p.state.SessionID), blob)
	if _, err := pipe.Exec(ctx); err != nil && !p.failedLog {
		// Once per call. A tenant with a wrong host would otherwise log on
		// every turn of every call and bury everything else.
		p.failedLog = true
		log.Printf("rexa: live publish failed for session=%s (further errors on this call suppressed): %v",
			p.state.SessionID, err)
	}
}

// asHash flattens the state for HSET. Fields are written even when empty so a
// reader never sees a stale value from an earlier update.
func (s LiveCallState) asHash() []any {
	return []any{
		"session_id", s.SessionID,
		"tenant_id", s.TenantID,
		"status", s.Status,
		"ccid", s.CCID,
		"join_url", s.JoinURL,
		"sentiment", s.Sentiment,
		"to_number", s.ToNumber,
		"from_number", s.FromNumber,
		"started_at", s.StartedAt,
		"updated_at", s.UpdatedAt,
	}
}

// String is for logs.
func (s LiveCallState) String() string {
	return fmt.Sprintf("session=%s status=%s sentiment=%s", s.SessionID, s.Status, s.Sentiment)
}
