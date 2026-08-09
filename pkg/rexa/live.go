package rexa

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"sync"
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

// LivePublisher appends call events to the caller's Redis.
//
// One per call, because the connection details arrive per dispatch. A nil
// publisher is valid and every method does nothing, which is what a dispatch
// without Redis details gets.
type LivePublisher struct {
	rdb *redis.Client
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
	return &LivePublisher{
		rdb: redis.NewClient(&redis.Options{
			Addr:         t.Host + ":" + strconv.Itoa(t.Port),
			DB:           t.DB,
			Password:     t.Password,
			DialTimeout:  livePublishTimeout,
			ReadTimeout:  livePublishTimeout,
			WriteTimeout: livePublishTimeout,
			// One connection per call: these are low-volume and short-lived,
			// and a larger pool would multiply sockets by concurrency.
			PoolSize: 1,
		}),
		keys: []string{sessionID},
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

	env := map[string]any{
		"event": name,
		// Unix seconds as a float — the consumer does
		// datetime.fromtimestamp(float(ts)), so milliseconds would date every
		// event to the year 57000 and an ISO string would fail the cast.
		"timestamp": float64(time.Now().UnixNano()) / 1e9,
	}
	for k, v := range extra {
		env[k] = v
	}
	blob, err := json.Marshal(env)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), livePublishTimeout)
	defer cancel()
	pipe := p.rdb.Pipeline()
	for _, k := range keys {
		pipe.RPush(ctx, k, blob)
		pipe.Expire(ctx, k, liveKeyTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil && !p.failedLog {
		// Once per call. A wrong host would otherwise log on every event of
		// every call and bury everything else.
		p.failedLog = true
		log.Printf("rexa: live publish failed for %v (further errors on this call suppressed): %v",
			keys, err)
	}
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
