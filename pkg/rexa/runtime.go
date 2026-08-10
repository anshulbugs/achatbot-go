package rexa

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Process health, for answering "is it leaking?" during a live campaign.
//
// Every number here is free to collect and none of it needs a profiler
// attached. That matters because leaks show up on the FIRST real campaign,
// which is exactly when nobody wants to be attaching a debugger to a process
// carrying live calls.
//
// WHAT A LEAK LOOKS LIKE, and why each number is here:
//
//   - goroutines climbing while calls.total stays flat. Every call starts
//     several (media read, media write, the sentiment classifier, the callback
//     poster), and every one of them must end when the call does. A goroutine
//     that outlives its call holds its whole frame graph — the session, the
//     transcript, the chat history — so this rises before memory does and is
//     the earliest warning available.
//   - heap growing across calls that have all ended. Steady-state heap should
//     return to roughly where it started once a campaign drains.
//   - RSS diverging from heap. That is not Go memory: it is the ONNX runtime
//     inside the VAD/ASR providers, or fragmentation. A pool that grows on
//     demand and never shrinks looks exactly like this.

// RuntimeSnapshot is the process's own state.
type RuntimeSnapshot struct {
	// Goroutines is the count now. The earliest leak signal there is.
	Goroutines int `json:"goroutines"`
	// HeapMB is Go heap in use.
	HeapMB int `json:"heap_mb"`
	// SysMB is memory Go has taken from the OS and not returned. It only ever
	// grows in practice, so a gap between this and HeapMB is Go's own
	// free-list, not a leak.
	SysMB int `json:"sys_mb"`
	// RSSMB is what the OS thinks the process is using — the number that
	// actually gets a container OOM-killed. Includes the ONNX runtime and every
	// native allocation Go knows nothing about. 0 where it cannot be read.
	RSSMB int `json:"rss_mb"`
	// OpenFDs catches socket and file leaks, which present as "too many open
	// files" long before memory becomes a problem. 0 where unreadable.
	OpenFDs int `json:"open_fds"`
	// GCPauseMs is the most recent stop-the-world pause. A pipeline that starts
	// stuttering under load has this as a suspect, and it is otherwise
	// invisible.
	GCPauseMs float64 `json:"gc_pause_ms"`
	// CachedAudio is what the pre-rendered greeting/voicemail cache is holding.
	//
	// Surfaced because it is the one structure that grows with CONTACTS rather
	// than with concurrency: the platform personalises greetings by name, so
	// every person dialled mints its own entry. It went unbounded for a long
	// time precisely because nothing reported it.
	CachedAudioEntries int `json:"cached_audio_entries"`
	CachedAudioMB      int `json:"cached_audio_mb"`
}

// AudioCacheStats is installed by the application so /health can report cache
// size without this package knowing what a greeting is. nil is fine.
var AudioCacheStats func() (entries, megabytes int)

// Runtime reads the current process state.
func Runtime() RuntimeSnapshot {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	out := RuntimeSnapshot{
		Goroutines: runtime.NumGoroutine(),
		HeapMB:     int(m.HeapAlloc / 1024 / 1024),
		SysMB:      int(m.Sys / 1024 / 1024),
		RSSMB:      residentMB(),
		OpenFDs:    openFDs(),
	}
	if m.NumGC > 0 {
		// PauseNs is a ring of the last 256 pauses, most recent at (NumGC+255)%256.
		out.GCPauseMs = float64(m.PauseNs[(m.NumGC+255)%256]) / 1e6
	}
	if AudioCacheStats != nil {
		out.CachedAudioEntries, out.CachedAudioMB = AudioCacheStats()
	}
	return out
}

// residentMB reads RSS from /proc/self/statm.
//
// statm rather than /proc/self/status: it is two numbers on one line instead of
// fifty labelled fields, so parsing it cannot be broken by a kernel adding a
// row. Returns 0 anywhere that is not Linux, which is every developer laptop
// and no production box.
func residentMB() int {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return int(pages * int64(os.Getpagesize()) / 1024 / 1024)
}

// openFDs counts entries in /proc/self/fd. Returns 0 where unreadable.
func openFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	// Minus one for the directory handle this call itself opened.
	return len(entries) - 1
}
