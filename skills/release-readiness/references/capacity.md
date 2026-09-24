# Capacity governance

Many lanes on one box will find its ceiling for you, at 3 a.m., as thrash, runc `setns` failures, SIGKILLed databases and false-green exit 0s. Govern capacity from the first wave.

## Find the real ceiling first

- **Host memory is not the ceiling when containers share a cgroup slice.** Read `memory.high`, `memory.max` and `memory.current` of the slice (Reservine: `devbox-docker.slice`, 40 GB high against 12 stacks of ~3 GB each). Above `memory.high` the kernel throttles every container, whatever `MemAvailable` says.
- **Check `pids.max` too.** Idle database threads hit it: ClickHouse's ~700 background threads per stack hit 8192.
- **On the Mac:** claude-guards' `simCap` (iOS simulators plus Android emulators) and `devServerCap` (Metro, dev servers), plus the SessionStart weather line. When it warns, start nothing heavy.
- **Admission queues can head-of-line block.** A strict-FIFO run queue sat behind standard-profile runs. Never hold a queue with a long-running server.

## The limiter

- Every heavy job goes through the adapter's `lane.heavy` wrapper: one browser/device batch, one build or one suite per invocation.
- When jobs run outside the dev env's own admission (e.g. `docker exec` over ssh into a lane stack), wrap them in `slot` from the run directory: `scp <run-dir>/slot <host>:~/readiness/slot`, then `ssh <host> 'bash ~/readiness/slot -- <cmd>'`.
- `slot` gates on:
  - a slot count (`READINESS_SLOTS`, default 3);
  - MemAvailable (`READINESS_MIN_AVAIL_GB`, 10);
  - the 1-min load (`READINESS_MAX_LOAD`, 48);
  - the slice headroom: all slots at ≥ 6 GB, one at 3–6 GB, none below.
  Its exit code is the command's. Jobs that run outside the slice set `READINESS_MIN_SLICE_HEADROOM_GB=-100`.

## Waves, park and GO

- **Waves.**
  - Wave A: stacks up, the finishers first.
  - Wave B: parked until wave A frees room (Reservine: ~15 GB of slice).
  - Wave C: not started.
  Log every transition in `rotation.md` with a UTC time.
- **Park** a stack the moment its lane waits on CI, an audit or a review (`devEnv.park`); the lane revives it with `devEnv.up`. Stacks running jobs stay up and are held (`devEnv.hold`).
- **When the box tightens**, re-pause the latest starters and let the finishers finish; then GO the queue in order. Send the `messages.md` "park" and "GO" texts; never improvise per-lane rules.
- **A lane may start a stack-free first phase** (reading, writing tests) while it waits for a GO.

## Processes

- Kill by recorded PID only: never `pkill`/`killall`/`pgrep` patterns, which kill other lanes' processes. Every wrapper script carries the lane slug in its name, so `ps` shows who owns what.
- Never re-run setup/up/gen on a stack that is already up: it can re-render units and restart other lanes' stacks. After a compose change, recreate only your own service.
- To free memory, a lane may stop its OWN dev server during API-only or suite phases, and restarts it before it finishes, so the stack is hand-testable.

## Validity

A run is invalid, and is re-run through the limiter, on any of:
- exit 137;
- an app or DB container restart during the run (`docker inspect -f '{{.RestartCount}} {{.State.StartedAt}}'`);
- a browser or simulator launch error (`pthread_create`, SIGABRT);
- a skip on an unreachable-dependency guard;
- a result count below the expected test count.

Anything that ran inside a known thrash window is suspect.

## The device runner (simulators and emulators)

With `devices.runner: device-runner`, one agent owns every simulator and emulator. It runs at most `devices.concurrent` of them (keep it ≤ the repo's `guards.simCap`), which is `concurrent / platforms` full matrix sets, serving that many lanes at once. Lanes queue through the orchestrator. The runner builds, runs every platform at the same time, runs realtime specs across platforms, and publishes to the lane's QA task. Night runs are the natural fit: the Mac is otherwise idle.
