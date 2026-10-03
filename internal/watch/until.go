package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Until exit codes: met, a failure, and the --timeout running out (124, as
// timeout(1) answers).
const (
	ExitMet     = 0
	ExitFailed  = 1
	ExitTimeout = 124
)

// UntilOptions is one blocking wait.
type UntilOptions struct {
	Target  string
	Until   string
	Dir     string
	Timeout time.Duration // 0 waits until met
	JSON    bool          // one event object per line instead of text
	Out     io.Writer
	Err     io.Writer
}

// sliceWait is one long-poll's length: short enough that nothing hangs
// unbounded, long enough to cost almost nothing.
const sliceWait = 25 * time.Second

// Until blocks until the condition holds, printing one line per state
// change. It subscribes through the daemon when one answers, and otherwise
// probes directly with a private engine (saying so on stderr), so a Monitor
// or a Codex lane never depends on the LaunchAgent being installed.
func Until(ctx context.Context, c *Client, probes []Probe, o UntilOptions) (int, error) {
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	session := "until-" + strings.TrimPrefix(newID(), "w")
	req := AddRequest{Session: session, Target: o.Target, Until: o.Until, Dir: o.Dir, TTL: o.Timeout + 10*time.Minute}
	if o.Timeout <= 0 {
		req.TTL = 0
	}
	res, err := c.Add(ctx, req)
	switch {
	case errors.Is(err, ErrDaemonDown):
		fmt.Fprintf(o.Err, "watch: %v; probing directly (vybava watch agent install runs the shared daemon)\n", err)
		return untilDirect(ctx, probes, req, o)
	case err != nil:
		return ExitFailed, err
	}
	defer func() {
		// The wait is over (met, timeout, interrupt): never leave the
		// subscription to poll for nobody until its TTL.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Remove(cleanup, res.Subscription.ID)
	}()
	after := int64(0)
	events := res.Events
	for {
		for _, ev := range events {
			after = max(after, ev.Seq)
			if done, code := report(o, ev); done {
				return code, nil
			}
		}
		events, err = c.Events(ctx, session, after, sliceWait)
		if err != nil {
			if ctx.Err() != nil {
				return finished(ctx)
			}
			return ExitFailed, err
		}
	}
}

func untilDirect(ctx context.Context, probes []Probe, req AddRequest, o UntilOptions) (int, error) {
	e, err := NewEngine(probes, Options{Log: o.Err})
	if err != nil {
		return ExitFailed, err
	}
	if _, err := e.Add(ctx, req); err != nil {
		return ExitFailed, err
	}
	after := int64(0)
	for {
		e.Tick(ctx)
		e.Wait()
		events, err := e.Events(ctx, req.Session, after, 0)
		if err != nil {
			if ctx.Err() != nil {
				return finished(ctx)
			}
			return ExitFailed, err
		}
		for _, ev := range events {
			after = max(after, ev.Seq)
			if done, code := report(o, ev); done {
				return code, nil
			}
		}
		next := e.NextDue()
		wait := time.Until(next)
		if next.IsZero() || wait < 0 {
			wait = 0
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return finished(ctx)
		}
	}
}

func finished(ctx context.Context) (int, error) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ExitTimeout, nil
	}
	return ExitFailed, ctx.Err()
}

// report prints one event and says whether the wait is over.
func report(o UntilOptions, ev Event) (bool, int) {
	if o.JSON {
		data, _ := json.Marshal(ev)
		fmt.Fprintln(o.Out, string(data))
	} else {
		line := fmt.Sprintf("%s %s %s", ev.At.UTC().Format(time.RFC3339), ev.Kind, ev.Target)
		switch {
		case ev.Error != "":
			line += " " + ev.Error
		case ev.Summary != "":
			line += " " + ev.Summary
		}
		fmt.Fprintln(o.Out, line)
	}
	switch ev.Kind {
	case EventMet:
		return true, ExitMet
	case EventExpired:
		return true, ExitTimeout
	case EventError:
		// The probe keeps backing off and may recover; the waiter is told,
		// not released.
		fmt.Fprintf(o.Err, "watch: %s keeps failing: %s\n", ev.Target, ev.Error)
	}
	return false, 0
}
