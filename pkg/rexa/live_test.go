package rexa

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The consumer is rexa-dialer's event_tailer.py. It tails a LIST with
// incremental LRANGE and routes on the envelope's `event` field, so these tests
// pin the two things it depends on: the list semantics and the wire shape.

// miniRedis is not available here, so these tests run against a real server
// when one is reachable and skip otherwise. The wire-shape test below needs no
// server and always runs.
func testClient(t *testing.T) (*LivePublisher, *redis.Client) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DialTimeout: 300 * time.Millisecond})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skip("no local redis on 127.0.0.1:6379")
	}
	key := "test-session-" + t.Name()
	rdb.Del(context.Background(), key)
	p := NewLivePublisher(RedisTarget{Host: "127.0.0.1", Port: 6379}, key)
	t.Cleanup(func() { rdb.Del(context.Background(), key); _ = rdb.Close() })
	return p, rdb
}

func TestEventsAppendInOrder(t *testing.T) {
	// The consumer reads forward by index. Order is the contract.
	p, rdb := testClient(t)
	key := "test-session-" + t.Name()

	p.Event(EventDialing, nil)
	p.Event(EventRinging, nil)
	p.Event(EventAnswered, nil)

	items, err := rdb.LRange(context.Background(), key, 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{EventDialing, EventRinging, EventAnswered}
	if len(items) != len(want) {
		t.Fatalf("got %d events, want %d", len(items), len(want))
	}
	for i, raw := range items {
		var env map[string]any
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Fatalf("element %d is not JSON: %v", i, err)
		}
		if env["event"] != want[i] {
			t.Fatalf("element %d = %v, want %v", i, env["event"], want[i])
		}
	}
}

func TestNothingIsPublishedAfterATerminalEvent(t *testing.T) {
	// The tailer exits its loop on call_ended, so a later push is invisible.
	// Appending anyway would grow the list and make our logs claim a delivery
	// nobody can read.
	p, rdb := testClient(t)
	key := "test-session-" + t.Name()

	p.Event(EventAnswered, nil)
	p.Ended("callee_hung_up")
	p.Event(EventRinging, nil) // must be dropped

	n, err := rdb.LLen(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("list length %d, want 2 — something was published after the terminal event", n)
	}
}

func TestBothKeysReceiveEveryEvent(t *testing.T) {
	// The consumer tails whichever id it stored from our dispatch response.
	// Publishing to both removes a whole class of "the wallboard shows
	// nothing" failure for one extra pipelined command.
	p, rdb := testClient(t)
	sessionKey := "test-session-" + t.Name()
	ccid := "v3:test-ccid"
	t.Cleanup(func() { rdb.Del(context.Background(), ccid) })

	p.AddKey(ccid)
	p.AddKey(ccid) // idempotent
	p.Event(EventAnswered, nil)

	for _, k := range []string{sessionKey, ccid} {
		n, err := rdb.LLen(context.Background(), k).Result()
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("key %q has %d events, want 1", k, n)
		}
	}
}

func TestTimestampIsUnixSecondsAsAFloat(t *testing.T) {
	// The consumer does datetime.fromtimestamp(float(ts)). Milliseconds would
	// date every event to the year 57000; an ISO string would fail the cast
	// and silently fall back to "now".
	env := map[string]any{}
	blob := envelopeFor(EventAnswered, nil)
	if err := json.Unmarshal(blob, &env); err != nil {
		t.Fatal(err)
	}
	ts, ok := env["timestamp"].(float64)
	if !ok {
		t.Fatalf("timestamp is %T, want a JSON number", env["timestamp"])
	}
	got := time.Unix(int64(ts), 0)
	if d := time.Since(got); d < 0 || d > time.Minute {
		t.Fatalf("timestamp decoded to %v (%v away from now) — wrong unit", got, d)
	}
}

func TestJoinDailyCarriesTheRoomBothWays(t *testing.T) {
	// The consumer looks for room_url at the envelope root OR in a nested
	// payload, because different builds of its sidecar wrote it each way. This
	// is a link an operator either gets or does not.
	blob := envelopeFor(EventJoinDaily, map[string]any{
		"room_url": "https://x.daily.co/abc",
		"payload":  map[string]any{"room_url": "https://x.daily.co/abc"},
	})
	var env map[string]any
	if err := json.Unmarshal(blob, &env); err != nil {
		t.Fatal(err)
	}
	if env["room_url"] != "https://x.daily.co/abc" {
		t.Fatal("room_url missing from the envelope root")
	}
	nested, _ := env["payload"].(map[string]any)
	if nested["room_url"] != "https://x.daily.co/abc" {
		t.Fatal("room_url missing from the nested payload")
	}
}

// The dispatch says where the Redis is, never whether it wants a password. Both
// wrong guesses produce a distinct error, and getting the correction backwards
// loses a whole call's worth of events to a one-word configuration difference.

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

func TestAuthFlipCorrectsBothWrongGuesses(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		usingPwd bool
		fallback string
		wantPwd  string
		wantFlip bool
	}{
		{
			name:     "we sent a password to an open server",
			err:      fakeErr("ERR Client sent AUTH, but no password is set"),
			usingPwd: true,
			fallback: "secret",
			wantPwd:  "",
			wantFlip: true,
		},
		{
			name:     "we sent nothing to a secured server",
			err:      fakeErr("NOAUTH Authentication required."),
			usingPwd: false,
			fallback: "secret",
			wantPwd:  "secret",
			wantFlip: true,
		},
		{
			name:     "secured server but we have no password to offer",
			err:      fakeErr("NOAUTH Authentication required."),
			usingPwd: false,
			fallback: "",
			wantFlip: false,
		},
		{
			name: "wrong password is not an auth-mode mistake",
			// Dropping the credential cannot fix this, and retrying
			// unauthenticated against a secured server just fails twice.
			err:      fakeErr("WRONGPASS invalid username-password pair"),
			usingPwd: true,
			fallback: "secret",
			wantFlip: false,
		},
		{
			name:     "an unreachable host is not an auth problem",
			err:      fakeErr("dial tcp 10.0.0.1:6379: i/o timeout"),
			usingPwd: false,
			fallback: "secret",
			wantFlip: false,
		},
		{
			name:     "no error, nothing to correct",
			err:      nil,
			usingPwd: true,
			fallback: "secret",
			wantFlip: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pwd, flip := authFlip(tc.err, tc.usingPwd, tc.fallback)
			if flip != tc.wantFlip {
				t.Fatalf("flip = %v, want %v", flip, tc.wantFlip)
			}
			if flip && pwd != tc.wantPwd {
				t.Fatalf("retry password = %q, want %q", pwd, tc.wantPwd)
			}
		})
	}
}
