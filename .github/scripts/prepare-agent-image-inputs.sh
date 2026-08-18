#!/usr/bin/env bash
set -euo pipefail

target_arch="${1:-}"
case "$target_arch" in
amd64 | arm64) ;;
*)
	echo "usage: $0 <amd64|arm64>" >&2
	exit 2
	;;
esac

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"
# shellcheck source=deps.env
. ./deps.env

version_no_v="${BR_VERSION#v}"
archive="br-v${version_no_v}-linux_${target_arch}.tar.gz"
case "$target_arch" in
amd64) expected_sha="aefc2ef6b16c7b275f6890636c110540c7bc081e203a1e8a706a376207d1f9dd" ;;
arm64) expected_sha="20899316274b7ac40de477f3318a3d6391f7885c6cd1bec7ba10e828360207fb" ;;
esac

scratch="$(mktemp -d -p /var/tmp gascity-agent-inputs.XXXXXX)"
trap 'rm -rf "$scratch"' EXIT
curl -fsSL --retry 3 \
	"https://github.com/Dicklesworthstone/beads_rust/releases/download/v${version_no_v}/${archive}" \
	-o "$scratch/$archive"
echo "$expected_sha  $scratch/$archive" | sha256sum --check --strict
tar -xzf "$scratch/$archive" -C "$scratch"
br_source="$(find "$scratch" -type f -name br -perm -111 | head -n 1)"
if [[ -z "$br_source" ]]; then
	echo "br executable missing from $archive" >&2
	exit 1
fi
install -m 0755 "$br_source" ./br

release_version="${GITHUB_REF_NAME:-dev}"
commit="${GITHUB_SHA:-$(git rev-parse HEAD)}"
build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
CGO_ENABLED=0 GOOS=linux GOARCH="$target_arch" go build \
	-trimpath \
	-ldflags="-s -w -X main.version=${release_version} -X main.commit=${commit} -X main.date=${build_date}" \
	-o ./gc \
	./cmd/gc

file ./gc ./br
