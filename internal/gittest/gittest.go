// Package gittest keeps every git a test binary starts from outliving the
// command that started it. A package whose tests, or the code they drive,
// run a history-writing git verb (commit, merge, fetch, pull, push, rebase,
// am, cherry-pick) against a t.TempDir repository calls NoDaemons from its
// TestMain:
//
//	func TestMain(m *testing.M) {
//		gittest.NoDaemons()
//		os.Exit(m.Run())
//	}
//
// Those verbs end by starting `git maintenance run --auto --detach`, a daemon
// the test cannot wait for. Since Git 2.54 its default strategy is geometric,
// which repacks as soon as objects/17/ holds two loose objects: a fixture
// whose commit hash lands in the same bucket as one of its blobs (one run in
// 256) gets a repack writing into .git/objects while t.TempDir's cleanup is
// deleting it, and the test fails with `TempDir RemoveAll cleanup: unlinkat
// …/.git: directory not empty`. core.fsmonitor=true in the machine's config
// starts another such daemon.
package gittest

import (
	"fmt"
	"os"
	"strconv"
)

// daemonConfig turns off every git feature that leaves a process behind its
// command. gc.auto=0 covers git before maintenance.auto (2.29), which ran
// `gc --auto` itself.
var daemonConfig = [][2]string{
	{"maintenance.auto", "false"},
	{"gc.auto", "0"},
	{"core.fsmonitor", "false"},
}

// NoDaemons appends daemonConfig to this process's GIT_CONFIG_COUNT /
// GIT_CONFIG_KEY_<n> / GIT_CONFIG_VALUE_<n>, which every git it starts
// inherits. Such entries have command-line scope, so no repository, global
// or system config file can turn a daemon back on. Entries already in the
// environment are kept.
func NoDaemons() {
	n := 0
	if s := os.Getenv("GIT_CONFIG_COUNT"); s != "" {
		var err error
		if n, err = strconv.Atoi(s); err != nil || n < 0 {
			panic(fmt.Sprintf("gittest: GIT_CONFIG_COUNT=%q is not a count", s))
		}
	}
	for _, kv := range daemonConfig {
		setenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", n), kv[0])
		setenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", n), kv[1])
		n++
	}
	setenv("GIT_CONFIG_COUNT", strconv.Itoa(n))
}

func setenv(key, value string) {
	if err := os.Setenv(key, value); err != nil {
		panic(fmt.Sprintf("gittest: %v", err))
	}
}
