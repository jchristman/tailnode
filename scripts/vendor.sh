#!/usr/bin/env bash
#
# Re-vendors dependencies and reapplies the local patches in patches/.
#
# vendor/ is committed and carries a patch to tailscale's netstack (see
# patches/0001-netstack-tcp-flow-verdict.patch), so `go mod vendor` on its own
# is not enough: it rewrites vendor/ from the module cache and silently drops
# the patch, which turns every dropped flow back into a reset. Run this instead
# of `go mod vendor`, and run it after any change to tailscale's version.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> go mod vendor"
go mod vendor

shopt -s nullglob
patches=(patches/*.patch)
if [[ ${#patches[@]} -eq 0 ]]; then
    echo "==> no patches to apply"
    exit 0
fi

for p in "${patches[@]}"; do
    echo "==> applying $p"
    if ! git apply --verbose "$p"; then
        echo
        echo "Failed to apply $p." >&2
        echo "A dependency bump usually means the patch needs rebasing against the" >&2
        echo "new upstream source; regenerate it and commit the result." >&2
        exit 1
    fi
done

echo "==> verifying the tree still builds"
go build ./...

echo "==> done"
