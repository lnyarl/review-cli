#!/bin/sh
# Build and install the review CLI (cmd/review). Re-run to update to the current checkout.
#   scripts/install-review.sh            # installs to ~/.local/bin/review
#   PREFIX=/usr/local/bin scripts/install-review.sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
prefix=${PREFIX:-$HOME/.local/bin}
target="$prefix/review"

command -v go >/dev/null 2>&1 || { echo "error: go is not installed" >&2; exit 1; }
mkdir -p "$prefix"
(cd "$root" && go build -o "$target" ./cmd/review)

# The CLI prints JSON when stdout is not a terminal; check that help parses as a usage document.
"$target" help | grep -q '"usage"' || { echo "error: $target help did not run" >&2; exit 1; }

commit=$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo unknown)
echo "installed: $target (commit $commit)"

case ":$PATH:" in
*":$prefix:"*) ;;
*) echo "warning: $prefix is not on PATH; add it to use 'review' directly" >&2 ;;
esac
if ! command -v claude >/dev/null 2>&1; then
	echo "warning: claude (Claude Code CLI) is not on PATH; review run needs it (or pass --claude PATH)" >&2
fi
