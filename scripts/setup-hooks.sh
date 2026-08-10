#!/usr/bin/env bash
#
# Points this clone's git hooks at the committed `.githooks/` directory.
#
# Run once per clone:
#
#     scripts/setup-hooks.sh
#
# Why this is a manual step, and what it means if nobody runs it
# --------------------------------------------------------------
# Git will not let a repository install its own hooks. `.git/hooks` is not
# tracked, and `core.hooksPath` is per-clone local config -- both by design,
# since a repo that could run code on clone would be a supply-chain attack in
# one line. So there is no version of this that is automatic, and pretending
# otherwise is how you end up with a hook everyone believes is running.
#
# The honest statement of what happens to someone who never runs this: nothing.
# No hook exists, `git commit` behaves exactly as it did before, and there is no
# warning.
#
# What still covers them, and what does not:
#
#   1. This hook -- catches it before the commit exists. Needs setup. It is the
#      only layer that prevents an incident rather than reporting one.
#   2. GitHub push protection -- would reject the push server side with no local
#      setup at all, and is exactly what would cover someone who skipped step 1.
#      NOT ENABLED: it needs paid Secret Protection on a private repo. See #109
#      and the README's "Why push protection is not on".
#   3. The `secret-scan` job in CI -- scans full history on every PR and is in
#      the `ci` aggregate's `needs`, so a finding blocks the merge.
#
# With 2 unavailable, skipping this script means a committed credential reaches
# GitHub before anything objects. Layer 3 then stops the merge, but the value is
# published by that point and has to be rotated. So this is not defence in depth
# at the moment -- for anything that gets pushed, it is the only prevention there
# is, and the 30 seconds are worth it.

set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"

# Any hook already in .git/hooks stops being run once core.hooksPath is set,
# rather than running alongside. Nothing here installs one today, but a tool
# might have, and silently disabling it would be a surprise worth avoiding.
existing="$(git rev-parse --git-path hooks)"
if [ -d "$existing" ]; then
	shadowed="$(find "$existing" -maxdepth 1 -type f ! -name '*.sample' -printf '%f\n' 2>/dev/null || true)"
	if [ -n "$shadowed" ]; then
		echo "warning: these hooks in ${existing} will no longer run:" >&2
		echo "$shadowed" | sed 's/^/  /' >&2
		echo "  (move them into .githooks/ if they are still wanted)" >&2
		echo >&2
	fi
fi

git config core.hooksPath .githooks
echo "core.hooksPath -> .githooks"

# Warm the gitleaks cache now, at a moment where a download is expected, rather
# than in the middle of someone's first commit.
echo "checking gitleaks..."
if ./scripts/gitleaks.sh version; then
	echo
	echo "Done. 'git commit' now scans staged changes for credentials."
	echo "To undo: git config --unset core.hooksPath"
else
	echo >&2
	echo "warning: the hook is installed but gitleaks could not be fetched." >&2
	echo "It will retry on your next commit; until it succeeds, commits are blocked." >&2
	exit 1
fi
