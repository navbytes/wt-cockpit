package client

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// idleWatchdog is how long Events tolerates a silent connection (no line —
// data or keepalive comment — read) before it gives up on the attempt and
// closes it. wtd keepalives every 15s (see cmd/wtd's handleEvents), so this
// comfortably survives a couple of missed beats before concluding the
// connection is dead rather than just quiet.
const idleWatchdog = 45 * time.Second

// Events opens exactly one connection to wtd's SSE stream and forwards every
// application event on the returned channel, doing nothing clever: no
// reconnect, no backoff, no resync policy — that's the caller's job (see
// internal/tui/conn.go). It returns once, immediately; both channels are
// closed together when the connection ends, for any reason (ctx cancelled,
// the server closed the stream, or the idle watchdog fired), with the
// terminal error (nil on a clean end) sent to errs exactly once beforehand.
//
// The handshake's "hello" named event (see cmd/wtd's sendSSEEvent) is
// consumed silently — its payload isn't shaped like model.Event, and the
// protocol handshake is verified separately via Version. Every other frame,
// named or not, is decoded as a model.Event (forward-compat: an event name
// this client has never heard of is still forwarded, never treated as fatal —
// see docs/02-stack-decision.md's "reversible" framing and P3-design.md §2.8).
func (c *Client) Events(ctx context.Context) (<-chan model.Event, <-chan error) {
	events := make(chan model.Event, 64)
	errs := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errs)

		reqCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.base+"/api/events", nil)
		if err != nil {
			errs <- err
			return
		}
		resp, err := c.sse.Do(req)
		if err != nil {
			errs <- &UnreachableError{Err: err}
			return
		}
		defer resp.Body.Close()

		// Any read (data or a ": keepalive" comment) resets the watchdog;
		// silence past it cancels this attempt so the caller sees an
		// ordinary connection failure and reconnects, same as any drop.
		watchdog := time.AfterFunc(idleWatchdog, cancel)
		defer watchdog.Stop()

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		var eventName string
		for sc.Scan() {
			watchdog.Reset(idleWatchdog)
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case line == "":
				eventName = ""
			case strings.HasPrefix(line, "data:"):
				if eventName == "hello" {
					continue
				}
				var e model.Event
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if err := json.Unmarshal([]byte(data), &e); err != nil {
					continue // a malformed frame shouldn't kill the whole stream
				}
				select {
				case events <- e:
				case <-reqCtx.Done():
					return
				}
			}
		}
		errs <- sc.Err()
	}()

	return events, errs
}
