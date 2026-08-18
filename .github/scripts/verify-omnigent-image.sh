#!/usr/bin/env bash
set -euo pipefail

image_ref="${1:-}"
if [[ -z "$image_ref" ]]; then
	echo "usage: $0 <image-ref>" >&2
	exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=deps.env
. "$repo_root/deps.env"

docker_args=(run --rm)
if [[ -n "${TARGET_ARCH:-}" ]]; then
	docker_args+=(--platform "linux/${TARGET_ARCH}")
fi
docker_args+=(
	-e "EXPECTED_OMNIGENT_VERSION=$OMNIGENT_VERSION"
	-e "EXPECTED_OMNIGENT_SOURCE_REF=$OMNIGENT_SOURCE_REF"
)

docker "${docker_args[@]}" "$image_ref" bash -lc '
set -euo pipefail
test ! -e /home/gcagent/.omnigent
test -r /usr/share/gascity/omnigent-provenance.env
. /usr/share/gascity/omnigent-provenance.env
test "$OMNIGENT_VERSION" = "$EXPECTED_OMNIGENT_VERSION"
test "$OMNIGENT_SOURCE_REF" = "$EXPECTED_OMNIGENT_SOURCE_REF"
actual="sha256:$(sha256sum /opt/omnigent/bin/omnigent | cut -d " " -f1)"
test "$actual" = "$OMNIGENT_EXECUTABLE_SHA256"
test "$OMNIGENT_NO_UPDATE_CHECK" = 1
test "$OMNIGENT_DISABLE_TELEMETRY" = true
test "$OMNIGENT_TELEMETRY_ENABLED" = 0
test "$OMNIGENT_OTEL_CAPTURE_CONTENT" = 0
test "$OMNIGENT_DISABLE_KEYRING" = 1
test -z "${OMNIGENT_AUTH_ENABLED:-}"
test -z "${OMNIGENT_REMOTE_AUTH_TOKEN:-}"
export OMNIGENT_CONFIG_HOME=/tmp/omnigent-smoke/config
export OMNIGENT_DATA_DIR=/tmp/omnigent-smoke/data
version_output="$(omnigent --version)"
case "$version_output" in
  "omnigent ${EXPECTED_OMNIGENT_VERSION} (${EXPECTED_OMNIGENT_SOURCE_REF:0:8},"*) ;;
  *) echo "unexpected Omnigent version: $version_output" >&2; exit 1 ;;
esac
omnigent server --help >/dev/null
/opt/omnigent/bin/python3 -c "import importlib.util; assert importlib.util.find_spec(\"daytona\") is None; assert importlib.util.find_spec(\"kubernetes\") is None"
gc version >/dev/null
BD_DISABLE_METRICS=1 BD_DISABLE_EVENT_FLUSH=1 bd --version >/dev/null
codex --version >/dev/null
br --version >/dev/null
printf "OMNIGENT_IMAGE_SMOKE_OK %s %s\n" "$OMNIGENT_SOURCE_REF" "$actual"
'
