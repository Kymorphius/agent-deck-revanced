#!/bin/bash
# matrix/hardened: build this fork and install it to ~/.local/bin/agent-deck.
#   scripts/matrix-install.sh [N]     N = our build number (default 1) -> <upstream tag>+matrix.N
# Upgrade flow: git fetch upstream --tags && git merge <new tag> (resolve conflicts keeping the
# matrix/hardened changes), run the changed-package tests with an isolated HOME, then this script.
# Never install the Homebrew tap again (token-bearing update checks, auto-update timer, telemetry).
set -euo pipefail
cd "$(dirname "$0")/.."
tag=$(git describe --tags --abbrev=0 --match 'v*' | sed 's/^v//')
ver="${tag}+matrix.${1:-1}"
out=$(mktemp -d)
GOTOOLCHAIN=${GOTOOLCHAIN:-go1.25.13} go build -trimpath -ldflags "-s -w -X main.Version=$ver" -o "$out/agent-deck" ./cmd/agent-deck
if strings "$out/agent-deck" | grep -qE 'phc_[A-Za-z0-9]{20}'; then echo "refusing: a PostHog key is compiled in" >&2; exit 1; fi
install -m 755 "$out/agent-deck" "$HOME/.local/bin/agent-deck"; rm -rf "$out"
# matrix/hardened: cross-built remote binaries must be read back on the target,
# rather than executed on the build host (the caller still stages the install).
if [[ "$(go env GOOS)/$(go env GOARCH)" != "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" ]]; then
  echo "installed cross-build: $ver ($(go env GOOS)/$(go env GOARCH)); verify on target"
  exit 0
fi
echo "installed: $("$HOME/.local/bin/agent-deck" --version)"
# matrix/hardened: consume all status output so pipefail does not report SIGPIPE
# after a successful install when hooks status writes more than three lines.
"$HOME/.local/bin/agent-deck" hooks status | sed -n '1,3p'
