# Local runtime observation protocol

`matrix/hardened:` this package provides one user-level observation owner for
TUI, Web and transition notification consumers. It is separate from the
per-profile core command daemon and exposes no session command execution,
terminal output, input, restart or hook-install endpoint.

The owner uses the private `runtime/monitor` directory beneath the effective
Agent Deck data root. The directory is mode 0700; its socket and flock file are
0600. Ownership paths reject symlinks, extra hard links, foreign UID and paths
outside this directory. Core's existing exclusive flock and dead-PID plus
refused-socket takeover remain authoritative. Both connection directions verify
the platform's Unix peer UID.

The wire transport is HTTP over Unix only. Requests and responses carry
`Agent-Deck-Monitor-Protocol: 1`. Unsupported versions, methods, routes, invalid
profiles and malformed cursors fail before creating sampling demand. The
endpoints are:

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/snapshot?profile=NAME` | The most recent complete profile publication |
| POST | `/refresh?profile=NAME` | Wait for a pass that starts after this request |
| GET | `/observations?profile=NAME&epoch=EPOCH&after=REVISION` | Ordered publications after the cursor |
| GET | `/status` | Version, PID, epoch and operational counters only |
| POST | `/stop` | Stop this observation process; AI panes remain running |

`matrix/hardened:` status also includes bounded, fixed-category tmux execution
counters for this process. Only real starts are counted. Completed synchronous
wrappers report cumulative wall time and waited client CPU; asynchronous starts
have no completion timing coverage. No command arguments, session names, socket
names, paths or pane content are exposed. Client CPU excludes parent fork and
the persistent tmux server; overlapping waits are not process CPU.

Every successful source publication has a protocol version, random process
epoch, monotonically increasing profile revision, profile and pass start time.
Each session carries its launch identity, its own probe start time and the
publication's envelope revision. Runtime DTOs include detailed substate, full
accepted hook evidence, activity and waiting times, acknowledgement with its
explicit timestamp, known
liveness, pane title and freshness, background work, auth hold, native bindings
and launch/model metadata. An observation error is explicit; a pending, failed,
stale or retired probe must never be treated as a fresh zero-valued verdict.

The initial empty cursor (`epoch=&after=0`) returns one latest baseline.
Subsequent cursors preserve chronological publications. Responses paginate at
16 MiB; consumers advance to the last returned revision and continue draining.
The bounded history retains at most 64 publications and 64 MiB. A retired epoch
or cursor produces HTTP 409 and `GapError`, also implementing the session
package's `RuntimeObservationGap` marker. Consumers explicitly establish a new
baseline on a gap; ordinary transport/source errors retain their cursor. They
must not fabricate completion transitions across an unknown interval.

Profile snapshot and observation requests renew a 30-second demand lease.
Expired profiles stop status collection and OpenCode streams; no active leases
also stops the hooks watcher. There is no profile enumeration or automatic
LaunchAgent installation. CLI callers provide a lazy-start callback that starts
their exact executable only for an absent/refused socket. Permission, protocol,
source and peer failures never start another collector. Closing a client only
closes that client's transport.

The collector retains profile-owned Storage and RegistryObserver connections.
Registry edits replace changed launch generations; ordinary producer status and
activity writes retain sampling state. Native/activity writes and status CAS
compare the persisted configuration watermark, so a concurrent stop or rebind
supersedes an older probe. Probes have a bounded worker pool and pass budget.
An unfinished call continues occupying its ID and worker slot even across a
registry reload; slow calls cannot multiply on every poll. Shutdown stops
publishing immediately and retains each registry owner until its remaining
legacy calls finish, rather than closing a DB underneath their writes.

`matrix/hardened:` small groups of reserved probe slots may prefetch raw captures
through a socket-local, read-only tmux command list. Groups contain at most four
probes, flush before waiting for more slots, and start independently across
sockets. Capture targets retain the existing current-window semantics. Random
framing must validate the complete batch before publishing any bytes; failure
falls back to ordinary individual capture. Raw-cache age starts at command
start and never extends the existing 500ms TTL. Independent/Fresh acquisition,
completion and invalidation fence older batch publications. Prefetch does not
classify, acknowledge, or stamp completion; original probes still perform those
actions. It creates no attach clients and does not alter window sizes.

`matrix/hardened:` while actual worker slots are saturated, a pending capture
group may coalesce adjacent completions within one cumulative 3ms window.
Groups holding all worker capacity flush immediately; deadline, collection
budget, cancellation and queue exhaustion also flush. This adds no idle timer
and does not change polling cadence, capture TTL or independent observation
actions. Status exposes fixed group-size/flush/wait counters, distinct from
actual tmux subprocess execution counters.

Regression coverage uses real Unix clients and the repository's isolated test
entry point. The external pane probe is replaced only where deterministic
hangs, demand expiry or generation changes are required; a two-profile test
uses real SQLite stores with identical management IDs to verify write ownership.

`matrix/hardened:` the production monitor alone opts into a dedicated read-only
tmux query channel. Stable registry order selects existing socket anchors;
registration is lazy and demand expiry/shutdown reaps only owned clients.
Control clients use `-E` and `ignore-size,no-output,read-only`; they add physical
attachment state and can trigger configured client lifecycle hooks, so native
acceptance checks those hooks, pane PIDs and geometry. Unknown query forms or
ambiguous/error responses fall back to native commands. Fresh stays native.
Status adds bounded transport counters and owned client PIDs; live client CPU
must be measured separately from already waited native child CPU.
