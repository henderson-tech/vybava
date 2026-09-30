# Human journey tooling

`doctor --plan /private/plan.json` and `devices --plan /private/plan.json`
validate the saved plan's seal/library and pass its explicit bindings to the
registered adapter. An optional binding `sessionId` pins an existing driver
session; adapter checks decide what that session proves. Raw session IDs stay
in the private plan and never appear in the sanitized attempt summary.
An optional binding `appiumPort` pins the loopback Appium server for that actor
when two devices use separate servers; `driverPort` still pins WDA or
UiAutomator2 on the device. The consumer adapter must verify both pins.

`journeys` is an experimental multicall applet. Markdown describes human
intent; no document can execute shell commands or perform app actions.

Private journal operations currently require macOS or Linux OS locking.
Windows builds retain document commands but reject journal access explicitly;
there is no unlocked write fallback.

Implemented: `lint`, `list`, `show`, `fmt`, `index`, `coverage`, `plan`, `case`,
`observe`, `verify`, `verdict`, `resume`, `finish`, `publish`, and the typed
`doctor`/`devices`/`start` adapter boundary. Live seed, native build and
publication transport belongs to the consumer adapter. The generic engine owns
the reviewed public projection, story pins and durable publication checkpoints.

Every command accepts `--root`, `--library` and `--json`. Record commands
require `--private` outside the repository. JSON envelopes contain `v`, `ok`,
`verb`, `data`, `diagnostics`, `next`. Exit 0 means success, 2 invalid input,
3 blocked environment/unsafe target/stale plan, 4 I/O, adapter or journal failure.
The executable must not be installed over a shared development build during a
trial; build a dedicated binary and invoke its `journeys` subcommand.

```sh
vybava journeys lint --root /path/to/project --json
vybava journeys fmt --check --root /path/to/project
vybava journeys index --root /path/to/project
vybava journeys coverage --root /path/to/project --json
vybava journeys show registered-customer-fix-now --root /path/to/project
```

The library uses `user-journeys/v1` flat YAML frontmatter, ordered `includes`,
vault-root wikilinks (aliases/headings/blocks), stable step IDs and GFM scenario
tables. The parser is [Goldmark](https://github.com/yuin/goldmark); YAML stays
in yaml.v3 nodes so extension fields and comments survive formatting. Mode
names come from journey documents. `all` expands only to those modes.
`INDEX.md` is a derived projection and is excluded from source hashes.

The plan input is the `Plan` JSON contract in `internal/journeys/records.go`:
an explicit list of cells and required checks, adapter name, operation list,
actor/device bindings, source/target fingerprints, origins and capabilities.
`plan --input draft.json --out plan.json` seals it with a new ID and hash.
Read-only document planning may omit runtime facts; `start` refuses that plan
until its bindings and adapter snapshot satisfy the mutation requirements.
Duplicate mode/scenario/topology/lane/condition tuples are rejected.

```sh
vybava journeys plan --input draft.json --out /private/plan.json
vybava journeys start /private/plan.json --apply --private /private/attempts
vybava journeys case /private/plan.json --phase preflight --private /private/attempts
vybava journeys observe ATTEMPT /private/event.json --private /private/attempts
vybava journeys verify ATTEMPT /private/check.json --private /private/attempts
vybava journeys verdict ATTEMPT /private/verdict.json --private /private/attempts
vybava journeys resume ATTEMPT --recover --private /private/attempts
vybava journeys finish ATTEMPT --export runs/ATTEMPT/summary.json --private /private/attempts
```

Events need a cell, actor, observed text and kind. `other` is the open vocabulary
escape hatch. Verification additionally needs a planned check, source, result
and evidence references. Money is integer minor units plus currency. A pass
requires evidenced customer and worker observations and all named checks
passing, including an independent backend check. This validates the evidence
record, not the truth of a human assertion; adapters/readers must inspect the
referenced evidence. It is not a cryptographic attestation of app behavior.

One advisory OS lock protects each attempt. Every append is fsynced. Resume
archives an incomplete final record before removing it; malformed complete
records are never repaired. A verdict closes its cell; finish closes the
attempt. Retests use `case --retest-of ATTEMPT`, retaining the run identity and
the failed attempt. Repeated finish verifies the journal and existing summary.
Saved plan output and journal roots must resolve outside the product repository,
including through symlinked ancestors. Raw journals are private, with owner-only permissions; callers must keep that
directory ignored by any enclosing export repository. Summaries omit prose,
device IDs, origins and private evidence paths. Exclusion reasons and approvers
are represented by hashes; their full text remains in the private journal.
Secret scanning is an additional gate, never permission to track raw evidence.

Coverage with no plan reports authored candidates and a zero execution count.
`coverage --plan plan.json --private /private/attempts` reports the explicit
denominator, latest attempt cell verdicts, exclusions, failures retained in
history, stale evidence and preflight attempts. Preflight never contributes
executed lifecycle cells. Scripted/checkpoint/API-assisted passes do not count
as full live passes. A missing or changed current adapter snapshot marks passes
stale. A bounded attempt leaves untouched cells' earlier results intact;
recording fresh evidence for a cell makes its prior pass pending until the
new attempt is finished. Explicit `not-run` verdicts do not erase a prior
substantive result.
The initial implementation reports cell coverage; richer
phase, push-state and money-rail pivots remain future projections.

`.journeys/adapters.json` is trusted repository configuration, separate from
frontmatter. It maps an adapter ID to a fixed argv array and explicit env-key
allowlist. Requests and receipts are versioned JSON on stdin/stdout; stderr is
withheld on protocol errors. `start` revalidates the snapshot and checkpoints
each operation. A retry carries the same `idempotencyKey`; the product adapter
must reconcile its own receipt before writing. A pending checkpoint is never
treated as success. No database ownership is inferred from a URL.

Focused tests are `go test -race ./internal/journeys` on Devbox. CLI registration
and catalog compatibility belong to `go test ./internal/cli ./internal/catalog`.

## Read an independent outcome

```sh
vybava journeys probe /private/plan.json /private/expectation.json --out /private/probe-001.json --json
```

`probe` checks the plan seal, library, bindings and current snapshot, then calls
the registered adapter's read-only `verify` operation. The consumer defines a
closed expectation schema; it cannot supply shell commands or arbitrary URLs.
The receipt binds the plan and compact request hashes, observation time, source,
scope and hashed evidence. Credentials belong in the adapter's allowed environment,
never the expectation. Output contains private actor/entity references; keep it
outside the repository and review/redact it before publication.

The output path is reserved exclusively before probing. Both failed and blocked
reads remain on disk. Reusing that path refuses another invocation; retry with a
new path. A crash can leave `state: pending`, which is never a successful outcome.
An expectation mismatch exits 2; a pending/unknown outcome exits 3; an invalid
adapter receipt exits 4. Adapter diagnostics distinguish unavailable prerequisites.
`verify ATTEMPT event.json` remains the explicit journal-recording command: cite
the probe receipt for the relevant planned check only. A probe neither appends
events nor establishes UI observations, readiness or a complete lifecycle pass.

## Publish a finished attempt

```sh
vybava journeys publish ATTEMPT --mode scheduled --presentation /private/presentation.json --private /private/attempts
vybava journeys publish ATTEMPT --mode scheduled --presentation /private/presentation.json --private /private/attempts --apply
# After inspecting every converted image named by the adapter's private receipt:
vybava journeys publish ATTEMPT --mode scheduled --presentation /private/presentation.json --private /private/attempts --apply --approve-capture SHA256 --export runs/ATTEMPT/publication-scheduled.json
```

Preview is read-only. A presentation is the versioned `Presentation` contract in
`internal/journeys/publication.go`: attempt ID, finished journal hash and selected
cells with public notes and explicitly reviewed image hashes. Each image must
reference evidence already recorded for the same cell and actor. Original
journal prose, actor IDs and private evidence references are not copied into
public notes. Images still need visual privacy review; a hash cannot redact pixels.
Pass publication requires reviewed customer and worker screenshots as well as
the finished attempt's checks. A preflight remains preflight in every projection.

New sealed plans pin each mode's story. Historical finished evidence can publish
after source or runtime changes without claiming current coverage. Older plans
without story pins require the original library hash. The consumer returns the
actual server run, case and board references, plus its exact handback. Failed
publication receipts remain pending; retries use the same projection hash and
idempotency key. A completed receipt is validated and returned without another
adapter call. Changed public copy requires a distinct attempt, not overwriting
the old publication. Optional `--export` writes a new sanitized references file
beside the immutable summary; it refuses overwrite and pending publication.
