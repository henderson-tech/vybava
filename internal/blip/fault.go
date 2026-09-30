package blip

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Fault is the one active fault of a proxy. It is the wire shape of `set`
// (CLI → control socket) and of the state file. Zero durations/rates mean
// "unset".
type Fault struct {
	Kind   string        `json:"kind"` // delay|drop|error|timeout|slow|flap
	Delay  time.Duration `json:"delay,omitempty"`
	Jitter time.Duration `json:"jitter,omitempty"`
	Status int           `json:"status,omitempty"`
	Body   string        `json:"body,omitempty"`
	Bytes  int64         `json:"bytes_per_sec,omitempty"`
	Down   time.Duration `json:"down,omitempty"`
	Up     time.Duration `json:"up,omitempty"`

	// Scoping.
	Match  string        `json:"match,omitempty"`
	Method string        `json:"method,omitempty"`
	After  int64         `json:"after,omitempty"`
	For    time.Duration `json:"for,omitempty"`
	Rate   float64       `json:"rate,omitempty"` // probability, default 1

	SetAt time.Time `json:"set_at,omitempty"`
}

// ScopeFlags are the flags every `set` accepts.
type ScopeFlags struct {
	Match, Method, For, Rate string
	After                    int64
	Jitter, Body             string
}

// String renders the fault the way `status` shows it.
func (f *Fault) String() string {
	if f == nil {
		return "none"
	}
	var s string
	switch f.Kind {
	case "delay":
		s = "delay " + f.Delay.String()
		if f.Jitter > 0 {
			s += " --jitter " + f.Jitter.String()
		}
	case "error":
		s = "error " + strconv.Itoa(f.Status)
	case "slow":
		s = fmt.Sprintf("slow %dbps", f.Bytes*8)
	case "flap":
		s = fmt.Sprintf("flap %s/%s", f.Down, f.Up)
	default:
		s = f.Kind
	}
	if f.Match != "" {
		s += " --match " + strconv.Quote(f.Match)
	}
	if f.Method != "" {
		s += " --method " + f.Method
	}
	if f.After > 0 {
		s += fmt.Sprintf(" --after %d", f.After)
	}
	if f.For > 0 {
		s += " --for " + f.For.String()
	}
	if f.Rate > 0 && f.Rate < 1 {
		s += fmt.Sprintf(" --rate %g", f.Rate)
	}
	return s
}

// ParseFault builds a Fault from `set <kind> [arg]` plus its scope flags.
// mode is "http" or "tcp": HTTP-only pieces on a tcp proxy are refused with
// the corrected invocation.
func ParseFault(name, mode string, args []string, flags ScopeFlags) (*Fault, error) {
	usage := "blip " + name + " set delay|drop|error|timeout|slow|flap …"
	if len(args) == 0 {
		return nil, diag(DiagFaultInvalid, "set needs a fault kind", usage)
	}
	f := &Fault{Kind: args[0], Rate: 1}
	arg := ""
	if len(args) > 1 {
		arg = args[1]
	}
	need := func(what string) error {
		if arg == "" {
			return diag(DiagFaultInvalid, f.Kind+" needs "+what, "blip "+name+" set "+f.Kind+" <"+what+">")
		}
		return nil
	}
	var err error
	switch f.Kind {
	case "delay":
		if err = need("duration"); err != nil {
			return nil, err
		}
		if f.Delay, err = ParseDuration(arg); err != nil {
			return nil, diag(DiagFaultInvalid, err.Error(), "blip "+name+" set delay 800ms")
		}
		if flags.Jitter != "" {
			if f.Jitter, err = ParseDuration(flags.Jitter); err != nil {
				return nil, diag(DiagFaultInvalid, err.Error(), "blip "+name+" set delay "+arg+" --jitter 400ms")
			}
		}
	case "drop", "timeout":
	case "error":
		if mode == "tcp" {
			return nil, diag(DiagHTTPOnly, "error is an HTTP fault; a tcp proxy can only drop, delay, timeout, slow or flap", "blip "+name+" set drop")
		}
		if err = need("status"); err != nil {
			return nil, err
		}
		if f.Status, err = strconv.Atoi(arg); err != nil || f.Status < 100 || f.Status > 599 {
			return nil, diag(DiagFaultInvalid, "not an HTTP status: "+strconv.Quote(arg), "blip "+name+" set error 503")
		}
		f.Body = flags.Body
	case "slow":
		if err = need("rate"); err != nil {
			return nil, err
		}
		if f.Bytes, err = ParseRate(arg); err != nil {
			return nil, diag(DiagFaultInvalid, err.Error(), "blip "+name+" set slow 20kbps")
		}
	case "flap":
		if err = need("down/up"); err != nil {
			return nil, err
		}
		if f.Down, f.Up, err = ParseFlap(arg); err != nil {
			return nil, diag(DiagFaultInvalid, err.Error(), "blip "+name+" set flap 5s/10s")
		}
	default:
		return nil, diag(DiagFaultInvalid, "unknown fault "+strconv.Quote(f.Kind), usage)
	}
	if mode == "tcp" && (flags.Match != "" || flags.Method != "") {
		return nil, diag(DiagHTTPOnly, "--match and --method scope HTTP requests; a tcp proxy has no paths or methods", "blip "+name+" set "+strings.Join(args, " "))
	}
	if flags.Match != "" {
		if _, err := globRE(flags.Match); err != nil {
			return nil, diag(DiagFaultInvalid, "bad --match glob: "+err.Error(), usage)
		}
		f.Match = flags.Match
	}
	f.Method = strings.ToUpper(flags.Method)
	f.After = flags.After
	if flags.For != "" {
		if f.For, err = ParseDuration(flags.For); err != nil {
			return nil, diag(DiagFaultInvalid, err.Error(), usage+" --for 30s")
		}
	}
	if flags.Rate != "" {
		if f.Rate, err = ParseProbability(flags.Rate); err != nil {
			return nil, diag(DiagFaultInvalid, err.Error(), usage+" --rate 0.5")
		}
	}
	return f, nil
}

// faultState is the daemon-side wrapper: the fault plus the counters the
// scoping needs. A `set` swaps in a fresh one, so --after restarts.
type faultState struct {
	Fault
	seen int64 // requests/connections that matched --match/--method
}

// expired reports whether --for has elapsed.
func (fs *faultState) expired(now time.Time) bool {
	return fs.For > 0 && now.Sub(fs.SetAt) >= fs.For
}

// decide answers whether the fault applies to this request/connection.
// method/path are empty in tcp mode. Every scoping rule is checked here and
// only here, so both proxies agree.
func (fs *faultState) decide(method, path string, now time.Time) bool {
	if fs == nil || fs.expired(now) {
		return false
	}
	if fs.Method != "" && fs.Method != method {
		return false
	}
	if !MatchPath(fs.Match, path) {
		return false
	}
	n := atomic.AddInt64(&fs.seen, 1)
	if n <= fs.After {
		return false
	}
	if fs.Kind == "flap" {
		cycle := fs.Down + fs.Up
		if now.Sub(fs.SetAt)%cycle >= fs.Down {
			return false // up window: pass through
		}
	}
	if fs.Rate > 0 && fs.Rate < 1 && rand.Float64() >= fs.Rate {
		return false
	}
	return true
}

// sleep is the delay plus a uniform jitter.
func (fs *faultState) sleep() time.Duration {
	d := fs.Delay
	if fs.Jitter > 0 {
		d += time.Duration(rand.Int64N(int64(fs.Jitter)))
	}
	return d
}
