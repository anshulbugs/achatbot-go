package rexa

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Live call events published to the caller's own Redis.
//
// THE SHAPE IS NOT OURS. It is dictated by the existing consumer
// (rexa-dialer's `apps/api/app/workers/event_tailer.py`), which tails a Redis
// LIST keyed by the call id we returned from the dispatch, reading forward with
// incremental `LRANGE key next_index -1`. Every design choice below follows
// from that:
//
//   - RPUSH to a LIST, not a hash and not pub/sub. The consumer polls by index
//     and replays from wherever it left off, so a subscriber that missed a
//     message has no way to recover but a list reader does.
//   - Append only. Never LSET, never LTRIM from the head: the consumer tracks
//     position by index, and removing an element shifts every later index down
//     so it silently skips one event and double-reads another.
//   - One JSON envelope per element: {"event": name, "timestamp": unix_float}.
//     The consumer json.loads each element and routes on `event`.
//
// Event vocabulary, also the consumer's:
//
//	call_answered      the carrier reports a human picked up
//	human_detected     a live pipeline is running — the AI has the call
//	machine_detected   answering machine; the consumer plays the voicemail path
//	join_daily         a live-listen room is ready, room_url in the payload
//	call_ended         terminal, carries end_reason
//	call_failed        terminal
//
// call_ended, call_failed and error are TERMINAL: the tailer exits its loop on
// them and stops reading. Anything published afterwards is never seen, which is
// why the room link and every status change must be pushed before the end.

// Live event names, matching the consumer's router exactly. A name it does not
// recognise is still stored as a CallEvent row, so extra events are harmless —
// but a misspelt one silently does nothing.
const (
	EventDialing         = "call_dialing"
	EventRinging         = "call_ringing"
	EventAnswered        = "call_answered"
	EventHumanDetected   = "human_detected"
	EventMachineDetected = "machine_detected"
	EventJoinDaily       = "join_daily"
	EventTransferred     = "call_transferred"
	EventEnded           = "call_ended"
	EventFailed          = "call_failed"
	// EventAISpeaking carries a `text` field into the consumer's live Console
	// snippet. It recognises several spellings of this
	// (ai_speaking / agent_speaking / tts_started / …); they are equivalent and
	// none of them changes the call's status.
	//
	// Sent ONCE per call, on the first thing the model says. Per turn would be
	// a Redis write for every reply on every call — real traffic at sixty
	// concurrent — to keep re-writing a line nobody is watching most of the
	// time. One event is what turns "this call is live" into "this call is
	// live and here is what it opened with".
	EventAISpeaking = "ai_speaking"
)

// liveKeyTTL bounds how long a call's event list survives.
//
// The consumer reads it during the call and has no use for it afterwards, but
// a crashed tailer must not leave lists accumulating in a tenant's Redis
// forever. Refreshed on every push, so a long call never expires under a reader.
const liveKeyTTL = 6 * time.Hour

// livePublishTimeout is the budget for one push. Deliberately tight: this runs
// on the call's own goroutines, and an unreachable Redis must cost the caller
// nothing.
const livePublishTimeout = 500 * time.Millisecond

// RedisTarget is the connection detail carried on a dispatch.
type RedisTarget struct {
	Host     string
	Port     int
	DB       int
	Password string
}

// Configured reports whether there is anywhere to publish to. It is also the
// signal that this call is being watched, which is what enables the expensive
// live-listening room.
func (t RedisTarget) Configured() bool { return t.Host != "" && t.Port > 0 }

// DefaultRedisPassword is used when a dispatch names a Redis host but carries
// no password.
//
// A bridge, not a design. Managed Redis (Render Key Value, ElastiCache with
// auth, Upstash) refuses unauthenticated connections, so a dispatch without a
// password reaches a server that closes the connection — and because publishing
// is fire-and-forget by design, that failure is invisible except for one log
// line. That is exactly the shape of "the wallboard shows nothing".
//
// Set REXA_REDIS_PASSWORD to cover the window before the platform's dispatch
// schema carries redis_password. A per-dispatch password always wins, so this
// stops mattering the moment they send one.
var DefaultRedisPassword string

// livePublishFailures counts calls whose event stream never reached Redis.
//
// Surfaced on /health precisely because the publish path is deliberately
// silent: an operator watching an empty wallboard needs somewhere to see that
// the agent tried and failed, rather than concluding the feature is missing.
var livePublishFailures atomic.Int64

// LivePublishFailures reports how many calls have failed to publish since
// start. Non-zero with live calls running means the Redis details on those
// dispatches are wrong or unreachable — most often a missing password.
func LivePublishFailures() int64 { return livePublishFailures.Load() }

// LivePublisher appends call events to the caller's Redis.
//
// One per call, because the connection details arrive per dispatch. A nil
// publisher is valid and every method does nothing, which is what a dispatch
// without Redis details gets.
type LivePublisher struct {
	rdb *redis.Client
	// target is kept so the connection can be rebuilt with the other
	// credential when the server disagrees with us about auth. See authFlip.
	target   RedisTarget
	usingPwd bool
	flipped  bool
	// keys are the lists to append to. Both the id we returned from the
	// dispatch and the session id: the consumer tails by whichever it stored
	// as its call_uuid, and writing to both costs one extra pipelined command
	// while removing a whole class of "the wallboard shows nothing" failure.
	mu   sync.Mutex
	keys []string

	failedLog bool
	// ended latches after a terminal event. The consumer stops reading at that
	// point, so anything pushed later is invisible — publishing it anyway
	// would just grow the list and make the log lie about what was delivered.
	ended bool
}

// NewLivePublisher connects to the caller's Redis. Returns nil when no target
// is configured, and nil is safe to call every method on.
//
// The dial happens lazily inside go-redis, so this never blocks a dispatch on
// an unreachable host — the first push absorbs the failure instead.
func NewLivePublisher(t RedisTarget, sessionID string) *LivePublisher {
	if !t.Configured() {
		return nil
	}
	password := t.Password
	if password == "" {
		password = DefaultRedisPassword
	}
	return &LivePublisher{
		rdb:      dialRedis(t, password),
		target:   t,
		usingPwd: password != "",
		keys:     []string{sessionID},
	}
}

func dialRedis(t RedisTarget, password string) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:         t.Host + ":" + strconv.Itoa(t.Port),
		DB:           t.DB,
		Password:     password,
		DialTimeout:  livePublishTimeout,
		ReadTimeout:  livePublishTimeout,
		WriteTimeout: livePublishTimeout,
		// One connection per call: these are low-volume and short-lived, and a
		// larger pool would multiply sockets by concurrency.
		PoolSize: 1,
	})
}

// authFlip decides whether a failure means we guessed the auth mode wrong, and
// what to try instead.
//
// Guessing is unavoidable: the dispatch says where the Redis is but not whether
// it wants a password, and the two failure modes are exact opposites.
//
//	open server, we sent AUTH   -> "ERR Client sent AUTH, but no password is set"
//	secured server, we sent none -> "NOAUTH Authentication required"
//
// Returns the password to retry with and whether to bother. A wrong password
// (WRONGPASS) is deliberately NOT flipped: dropping the password would not fix
// it, and retrying unauthenticated against a secured server just fails twice.
func authFlip(err error, usingPwd bool, fallback string) (string, bool) {
	if err == nil {
		return "", false
	}
	msg := err.Error()
	switch {
	case usingPwd && strings.Contains(msg, "but no password is set"):
		// The server is open. Drop the credential.
		return "", true
	case !usingPwd && strings.Contains(msg, "NOAUTH") && fallback != "":
		// The server wants auth and we have something to offer.
		return fallback, true
	default:
		return "", false
	}
}

// AddKey registers the carrier call id as a second list to publish to, once the
// dial has produced one. Idempotent.
func (p *LivePublisher) AddKey(k string) {
	if p == nil || k == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, existing := range p.keys {
		if existing == k {
			return
		}
	}
	p.keys = append(p.keys, k)
}

// Event appends one envelope. extra is merged into the envelope root, which is
// where the consumer looks first for fields like end_reason and room_url.
func (p *LivePublisher) Event(name string, extra map[string]any) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.ended {
		p.mu.Unlock()
		return
	}
	if name == EventEnded || name == EventFailed {
		p.ended = true
	}
	keys := append([]string(nil), p.keys...)
	p.mu.Unlock()

	blob := envelopeFor(name, extra)
	if blob == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), livePublishTimeout)
	defer cancel()
	pipe := p.rdb.Pipeline()
	for _, k := range keys {
		pipe.RPush(ctx, k, blob)
		pipe.Expire(ctx, k, liveKeyTTL)
	}
	_, err := pipe.Exec(ctx)

	// The dispatch tells us where the Redis is, never whether it wants a
	// password. Rather than fail every event on a guess, correct the guess once
	// and replay this event — the alternative is a whole call's worth of events
	// lost to a one-word configuration difference.
	if err != nil && !p.flipped {
		if pwd, flip := authFlip(err, p.usingPwd, DefaultRedisPassword); flip {
			p.flipped = true
			_ = p.rdb.Close()
			p.rdb = dialRedis(p.target, pwd)
			p.usingPwd = pwd != ""
			log.Printf("rexa: live publish retrying with auth %s for %v",
				map[bool]string{true: "enabled", false: "disabled"}[p.usingPwd], keys)
			retry := p.rdb.Pipeline()
			for _, k := range keys {
				retry.RPush(ctx, k, blob)
				retry.Expire(ctx, k, liveKeyTTL)
			}
			_, err = retry.Exec(ctx)
		}
	}

	if err != nil && !p.failedLog {
		// Once per call. A wrong host would otherwise log on every event of
		// every call and bury everything else. The counter is what makes the
		// failure visible on /health, since the log line alone is easy to miss
		// and the publish path is otherwise silent by design.
		p.failedLog = true
		livePublishFailures.Add(1)
		log.Printf("rexa: live publish FAILED for %v — the caller sees no live events "+
			"for this call (auth? host reachable?); further errors on this call suppressed: %v",
			keys, err)
	}
}

// envelopeFor builds one list element.
func envelopeFor(name string, extra map[string]any) []byte {
	env := map[string]any{
		"event": name,
		// Unix seconds as a float: the consumer does
		// datetime.fromtimestamp(float(ts)), so milliseconds would date every
		// event to the year 57000 and an ISO string would fail the cast and
		// silently fall back to "now".
		"timestamp": float64(time.Now().UnixNano()) / 1e9,
	}
	for k, v := range extra {
		env[k] = v
	}
	blob, err := json.Marshal(env)
	if err != nil {
		return nil
	}
	return blob
}

// JoinDaily publishes the live-listen room.
//
// The consumer reads room_url from the envelope root or from a nested payload;
// both are sent because different builds of its sidecar wrote it each way, and
// this is a link an operator either gets or does not.
func (p *LivePublisher) JoinDaily(roomURL, token string) {
	if p == nil || roomURL == "" {
		return
	}
	payload := map[string]any{"room_url": roomURL}
	if token != "" {
		payload["token"] = token
	}
	p.Event(EventJoinDaily, map[string]any{
		"room_url": roomURL,
		"token":    token,
		"payload":  payload,
	})
}

// Ended publishes the terminal event and closes the connection.
//
// end_reason "voicemail" is special to the consumer: it routes the call into
// the voicemail bucket rather than treating it as completed, so a machine
// answer must report it here as well as via machine_detected.
func (p *LivePublisher) Ended(reason string) {
	if p == nil {
		return
	}
	p.Event(EventEnded, map[string]any{"end_reason": reason})
	_ = p.rdb.Close()
}

// Failed publishes a terminal failure and closes the connection.
func (p *LivePublisher) Failed(reason string) {
	if p == nil {
		return
	}
	p.Event(EventFailed, map[string]any{"end_reason": reason})
	_ = p.rdb.Close()
}
