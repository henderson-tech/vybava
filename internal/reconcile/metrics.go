package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Metrics renders the node-exporter textfile for one result. The staleness
// alert in devulinka keys on last_tick_timestamp: a skipped tick (stuck lock,
// dead cron) stops advancing it.
func Metrics(r Result, now int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP infra_reconcile_last_tick_timestamp Unix time of the last completed reconcile tick.\n")
	fmt.Fprintf(&b, "# TYPE infra_reconcile_last_tick_timestamp gauge\n")
	fmt.Fprintf(&b, "infra_reconcile_last_tick_timestamp %d\n", now)
	fmt.Fprintf(&b, "# HELP infra_reconcile_pending Files whose repo version is not live yet.\n")
	fmt.Fprintf(&b, "# TYPE infra_reconcile_pending gauge\n")
	fmt.Fprintf(&b, "infra_reconcile_pending %d\n", len(r.Pending))
	fmt.Fprintf(&b, "# HELP infra_reconcile_held Live files hand-edited since the last apply (never overwritten).\n")
	fmt.Fprintf(&b, "# TYPE infra_reconcile_held gauge\n")
	fmt.Fprintf(&b, "infra_reconcile_held %d\n", len(r.Held))
	fmt.Fprintf(&b, "# HELP infra_reconcile_cert_held TLS vhosts held because their certificate is not on the box yet.\n")
	fmt.Fprintf(&b, "# TYPE infra_reconcile_cert_held gauge\n")
	fmt.Fprintf(&b, "infra_reconcile_cert_held %d\n", len(r.CertHeld))
	fmt.Fprintf(&b, "# HELP infra_reconcile_errors Errors (incl. failed hooks) in the last tick.\n")
	fmt.Fprintf(&b, "# TYPE infra_reconcile_errors gauge\n")
	fmt.Fprintf(&b, "infra_reconcile_errors %d\n", len(r.Errors)+len(r.FailedHooks))
	fmt.Fprintf(&b, "# HELP infra_reconcile_mode_info The mode the last tick ran in.\n")
	fmt.Fprintf(&b, "# TYPE infra_reconcile_mode_info gauge\n")
	fmt.Fprintf(&b, "infra_reconcile_mode_info{mode=%q} 1\n", r.Mode)
	fmt.Fprintf(&b, "# HELP infra_reconcile_last_good_commit_info The last commit that converged fully with hooks passing.\n")
	fmt.Fprintf(&b, "# TYPE infra_reconcile_last_good_commit_info gauge\n")
	fmt.Fprintf(&b, "infra_reconcile_last_good_commit_info{sha=%q} 1\n", r.LastGood)
	return b.String()
}

func (e *Engine) writeMetrics(r Result) error {
	if e.M.MetricsFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(e.M.MetricsFile), 0o755); err != nil {
		return err
	}
	return atomicWrite(e.M.MetricsFile, []byte(Metrics(r, e.now().Unix())), 0o644)
}
