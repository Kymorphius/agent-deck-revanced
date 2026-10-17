# Agent Deck Revanced

This repository contains an independently maintained source snapshot of Agent Deck with the `matrix/hardened` privacy changes and session-management extensions. The upstream project is [asheshgoplani/agent-deck](https://github.com/asheshgoplani/agent-deck); its MIT license and attribution are preserved in [LICENSE](LICENSE).

The initial snapshot starts a new Git history. Earlier branches, tags, and commits mentioned in historical development notes are provenance references and are not included in this repository's history.

## Build and verify

The executable and Go module retain the `agent-deck` name for compatibility. From this repository's root:

```sh
# Build without installing or changing the running application.
go build -o /tmp/agent-deck-revanced ./cmd/agent-deck

# Run Go tests with isolated runtime state and shared compiler caches.
scripts/run-isolated.py go test ./internal/tmux/...
```

For installation, use `scripts/matrix-install.sh` as described in [MATRIX-HARDENED.md](MATRIX-HARDENED.md). Upstream release packages do not contain this repository's hardening changes.

## Privacy and publication

The source snapshot excludes local machine notes, local brainstorming documents, agent handoff plans, recorded demo media, a machine-specific stress runner, ignored runtime files, credentials, and the original Git history. Existing external links to upstream demonstrations remain upstream references. Terminal regression fixtures retain their diagnostic structure with personal payloads and identifying filenames sanitized. Example host names in the added group-search tests and documentation use neutral names.

Credential-like literals required by security and protocol tests are synthetic test data. They are retained so those tests continue to exercise credential detection and authentication framing.

The privacy requirements R1–R8 and the historical feature notes are documented in [MATRIX-HARDENED.md](MATRIX-HARDENED.md).

## Snapshot verification

The initial export is based on source commit `ebb7f24e`. The command-line build, all 117 pane-status fixture verdicts, and the targeted group-search and session-interruption tests passed.

The additional integration case `TestForeignTmuxServer_NestedTUIDoesNotFlipLiveSessionsToError/session-gone` fails in both the original source and this snapshot: the nested TUI keeps a closed session marked as running. This is an inherited validation limitation.
