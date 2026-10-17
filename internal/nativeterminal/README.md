# Native terminal adapter

`Terminal` implements `terminalbackend.Backend` for a durable Windows native
fleet run. The original session layer can depend on that shared interface while
the transport reuses the existing fleet, ConPTY host, and named-pipe client.

The caller first verifies the manager's protocol, build version and state
namespace, then selects explicit session and run UUIDs. `New(pipe, sessionID,
runID)` validates and fixes that binding. The adapter does not start a manager,
start a session, discover a current run, or retry a failed request. Each operation
opens a local `winipc.Dial` connection, which verifies the server's current-user
SID, and includes both UUIDs. A different generation requires an explicit new
binding made by the higher layer.

`Observe` reports process facts. `Alive` is the host's process-liveness observation
and does not mean an AI is working, waiting, or ready for a prompt. Cancelling a
request closes its one client connection; stopping is a separate explicit
operation fenced to that run. Stop rejects a successful reply that still reports
a live process. A host observation diagnostic returns the observed facts together
with `ErrObservation`, so the session layer can preserve uncertainty rather than
silently discarding the diagnostic.

`Capture(ctx, false)` returns the host's emulated current `Screen` as plain text.
It never uses the raw output or ANSI `Render` as a preview. `Capture(ctx, true)`
reads the initial retained-output window, capped at the host's 4 MiB ring and
bounded request count, then strips terminal controls. Bytes produced after the
initial output watermark are excluded. History eviction while paging is an
explicit error. This retained output is not a complete transcript or reconstructed
terminal scrollback; carriage-return rewrites appear as separate lines. Neither
capture path replays terminal device queries, clipboard commands, or other VT
controls in the viewing terminal.

`NewWithCaller` supplies the same contract for offline protocol tests. Its caller
must preserve server verification, client ownership and write-uncertainty
semantics; this injection point does not establish a trusted manager handshake.

All implementation and tests here are `matrix/hardened` integration work and leave
the existing macOS/Linux tmux runtime unchanged.
