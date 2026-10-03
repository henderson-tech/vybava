package cmux

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Event is one frame of cmux's event stream.
type Event struct {
	Seq         int64           `json:"seq"`
	ID          string          `json:"id"`
	BootID      string          `json:"boot_id"`
	Name        string          `json:"name"`
	Category    string          `json:"category"`
	SurfaceID   string          `json:"surface_id"`
	WorkspaceID string          `json:"workspace_id"`
	OccurredAt  time.Time       `json:"occurred_at"`
	Payload     json.RawMessage `json:"payload"`
}

// Ack opens every subscription; Resume says whether events were lost since
// the requested sequence.
type Ack struct {
	BootID string `json:"boot_id"`
	Resume struct {
		Gap       bool  `json:"gap"`
		LatestSeq int64 `json:"latest_seq"`
		NextSeq   int64 `json:"next_seq"`
		OldestSeq int64 `json:"oldest_seq"`
	} `json:"resume"`
}

type frame struct {
	Type string `json:"type"`
	Event
}

// heartbeatGrace is how long a stream may stay silent: cmux sends a
// heartbeat every 15 s when nothing happens, so three missed ones mean the
// connection is dead even if the socket did not say so.
const heartbeatGrace = 45 * time.Second

// Stream is one live events.stream subscription; it owns its connection.
type Stream struct {
	conn net.Conn
	r    *bufio.Reader
	Ack  Ack
}

// Subscribe opens a stream. after > 0 replays retained events after that
// sequence; 0 starts at the live edge. Empty categories subscribe to all.
func (c Client) Subscribe(ctx context.Context, after int64, categories []string) (*Stream, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	params := map[string]any{"include_heartbeats": true}
	if after > 0 {
		params["after_seq"] = after
	}
	if len(categories) > 0 {
		params["categories"] = categories
	}
	_ = conn.SetDeadline(c.deadline(ctx))
	if err := json.NewEncoder(conn).Encode(request{ID: "vybava-events", Method: "events.stream", Params: params}); err != nil {
		conn.Close()
		return nil, &UnreachableError{Socket: c.Socket, Err: err, Closed: true}
	}
	s := &Stream{conn: conn, r: bufio.NewReaderSize(conn, 64<<10)}
	line, err := readLine(s.r)
	if err != nil {
		conn.Close()
		return nil, &UnreachableError{Socket: c.Socket, Err: err, Closed: true}
	}
	var first struct {
		Type string `json:"type"`
		Ack
		response
	}
	if err := json.Unmarshal(line, &first); err != nil {
		conn.Close()
		return nil, fmt.Errorf("cmux events.stream ack: %w", err)
	}
	if first.Type != "ack" {
		conn.Close()
		if first.Error != nil {
			return nil, &Error{Method: "events.stream", Code: first.Error.Code, Message: first.Error.Message}
		}
		return nil, fmt.Errorf("cmux events.stream: first frame is %q, not an ack", first.Type)
	}
	s.Ack = first.Ack
	return s, nil
}

// Next returns the next event, skipping heartbeats. An error ends the stream.
func (s *Stream) Next() (Event, error) {
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(heartbeatGrace))
		line, err := readLine(s.r)
		if err != nil {
			return Event{}, err
		}
		var f frame
		if err := json.Unmarshal(line, &f); err != nil {
			return Event{}, fmt.Errorf("cmux event frame: %w", err)
		}
		switch f.Type {
		case "heartbeat":
			continue
		case "event", "":
			return f.Event, nil
		default:
			return Event{}, fmt.Errorf("cmux events.stream ended: %s", line)
		}
	}
}

// Close ends the subscription.
func (s *Stream) Close() error { return s.conn.Close() }

// Follow keeps a subscription alive until ctx ends: it reconnects after the
// last sequence it delivered and calls gap whenever events may have been
// lost — the ack says so, or cmux restarted (its boot id changed, and
// sequences restart with it). The caller refreshes its whole view on gap;
// the first connect is not a gap. Reconnects back off from 1 s to 30 s.
func (c Client) Follow(ctx context.Context, categories []string, handle func(Event), gap func(reason string)) error {
	var last int64
	var boot string
	backoff := time.Second
	for ctx.Err() == nil {
		stream, err := c.Subscribe(ctx, last, categories)
		if err != nil {
			if !c.wait(ctx, backoff) {
				break
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		switch {
		case boot != "" && stream.Ack.BootID != boot:
			last = 0
			gap("cmux restarted")
		case last > 0 && stream.Ack.Resume.Gap:
			gap(fmt.Sprintf("events after %d no longer retained", last))
		}
		boot = stream.Ack.BootID
		stop := context.AfterFunc(ctx, func() { stream.Close() })
		for {
			event, err := stream.Next()
			if err != nil {
				break
			}
			if event.Seq > last {
				last = event.Seq
			}
			handle(event)
		}
		stop()
		stream.Close()
	}
	return ctx.Err()
}

func (c Client) wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
