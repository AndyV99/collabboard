#!/usr/bin/env bash
#
# The single entry point for running gitleaks in this repo.
#
# CI (.github/workflows/ci.yml) and the pre-commit hook (.githooks/pre-commit)
# both go through this script, which is the whole point of it existing: the
# version, the checksum and the config file are resolved in exactly one place,
# so a local "clean" and a CI "clean" cannot mean different things. If CI
# installed gitleaks its own way -- an action, apt, brew, `go install` -- the
# two would drift the first time either side moved, and the failure mode is
# silent: the hook passes, CI passes, and a rule that only exists in one of them
# never runs.
#
# Usage:
#   scripts/gitleaks.sh git --no-banner .     # scan full commit history
#   scripts/gitleaks.sh git --staged          # scan what is staged (the hook)
#
# All arguments are passed through to gitleaks. --config is injected here and
# does not need to be supplied.
#
# Exit codes are gitleaks': 0 clean, 1 findings, anything else an error.

set -euo pipefail

# Pinned rather than floating, for the same reason every other tool version in
# ci.yml is pinned: a gitleaks release that adds or tightens a rule would
# otherwise turn a green pipeline red with no commit that says so, and -- worse
# for a hook -- would do it on one developer's machine before the others.
#
# Bump deliberately: change the version, replace all four checksums from
# https://github.com/gitleaks/gitleaks/releases/download/vX.Y.Z/gitleaks_X.Y.Z_checksums.txt
# and re-run a full-history scan before pushing, since new rules find old
# commits.
GITLEAKS_VERSION="8.30.1"

# sha256 of the release tarballs, from the checksums.txt published with the
# release. Pinning the version alone would still accept a retagged asset; these
# make the download's contents part of the pin. Only the platforms anyone here
# actually develops or builds on are listed -- an unlisted one fails loudly
# rather than silently skipping verification.
checksum_for() {
	case "$1" in
	linux_x64) echo "551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb" ;;
	linux_arm64) echo "e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080" ;;
	darwin_x64) echo "dfe101a4db2255fc85120ac7f3d25e4342c3c20cf749f2c20a18081af1952709" ;;
	darwin_arm64) echo "b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5" ;;
	*) return 1 ;;
	esac
}

repo_root() {
	git rev-parse --show-toplevel 2>/dev/null || {
		# Fall back to the script's own location so this still works outside a
		# work tree (it is only ever invoked from inside one in practice).
		cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd
	}
}

platform() {
	local os arch
	case "$(uname -s)" in
	Linux) os="linux" ;;
	Darwin) os="darwin" ;;
	*)
		echo "gitleaks.sh: unsupported OS '$(uname -s)'." >&2
		echo "Add its release asset name and checksum to checksum_for() above." >&2
		return 1
		;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch="x64" ;;
	arm64 | aarch64) arch="arm64" ;;
	*)
		echo "gitleaks.sh: unsupported architecture '$(uname -m)'." >&2
		echo "Add its release asset name and checksum to checksum_for() above." >&2
		return 1
		;;
	esac
	echo "${os}_${arch}"
}

# Installs the pinned build into a per-version cache directory and prints its
# path. Keyed on the version so a bump installs alongside rather than over the
# old one, and so the check below is an existence test rather than a
# `--version` parse.
ensure_gitleaks() {
	local plat cache_dir bin url want_sum tmp
	plat="$(platform)"
	cache_dir="${XDG_CACHE_HOME:-${HOME}/.cache}/collabboard/gitleaks/${GITLEAKS_VERSION}"
	bin="${cache_dir}/gitleaks"

	if [ -x "$bin" ]; then
		echo "$bin"
		return 0
	fi

	if ! want_sum="$(checksum_for "$plat")"; then
		echo "gitleaks.sh: no pinned checksum for platform '${plat}'." >&2
		return 1
	fi

	url="https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/gitleaks_${GITLEAKS_VERSION}_${plat}.tar.gz"

	# Only the first run on a machine needs the network; every later commit hits
	# the cache. A failure here is fatal on purpose -- see the hook for why a
	# scanner that quietly does nothing is worse than no scanner.
	echo "gitleaks.sh: installing gitleaks v${GITLEAKS_VERSION} (${plat}) into ${cache_dir}" >&2

	tmp="$(mktemp -d)"
	# shellcheck disable=SC2064 # expand tmp now, not at trap time
	trap "rm -rf '${tmp}'" RETURN

	if ! curl -fsSL --retry 3 --retry-delay 2 -o "${tmp}/gitleaks.tar.gz" "$url"; then
		echo "gitleaks.sh: failed to download ${url}" >&2
		return 1
	fi

	local got_sum
	got_sum="$(sha256sum "${tmp}/gitleaks.tar.gz" 2>/dev/null | cut -d' ' -f1)" ||
		got_sum="$(shasum -a 256 "${tmp}/gitleaks.tar.gz" | cut -d' ' -f1)"

	if [ "$got_sum" != "$want_sum" ]; then
		echo "gitleaks.sh: checksum mismatch for gitleaks v${GITLEAKS_VERSION} ${plat}" >&2
		echo "  expected ${want_sum}" >&2
		echo "  got      ${got_sum}" >&2
		return 1
	fi

	tar -xzf "${tmp}/gitleaks.tar.gz" -C "${tmp}" gitleaks
	mkdir -p "$cache_dir"
	# Move into place as one step, so a concurrent run never sees a half-written
	# binary at the cached path.
	mv "${tmp}/gitleaks" "${bin}.tmp.$$"
	chmod +x "${bin}.tmp.$$"
	mv "${bin}.tmp.$$" "$bin"

	echo "$bin"
}

main() {
	local root bin
	root="$(repo_root)"
	bin="$(ensure_gitleaks)"
	exec "$bin" --config "${root}/.gitleaks.toml" "$@"
}

main "$@"
