#!/usr/bin/env bash
# Run the real-Envoy contract suite without allowing a green skip.
# Supply independently verified official 1.39.6 and 1.33.2 Linux binaries.
set -euo pipefail
: "${AIBRIX_ENVOY_TEST_BINARY:?set to a verified Envoy 1.39.6 executable}"
: "${AIBRIX_ENVOY_BASELINE_TEST_BINARY:?set to a verified Envoy 1.33.2 executable}"
for binary in "$AIBRIX_ENVOY_TEST_BINARY" "$AIBRIX_ENVOY_BASELINE_TEST_BINARY"; do
  test -x "$binary" || { printf 'Not executable: %s\n' "$binary" >&2; exit 1; }
done
# Do not let an inherited Docker image take precedence over the supplied binary.
unset AIBRIX_ENVOY_TEST_IMAGE
cd "$(dirname "$0")/.."
log=$(mktemp)
trap 'rm -f "$log"' EXIT
go test -json ./pkg/plugins/gateway \
  -run '^TestEnvoy(StreamedUpstreamErrors|GeneratedFailureCallbacks|GeneratedFailureBaseline)$' \
  -count=1 -timeout=120s | tee "$log"
python3 - "$log" <<'PY'
import json
import sys
expected = {
    'TestEnvoyStreamedUpstreamErrors',
    'TestEnvoyGeneratedFailureCallbacks',
    'TestEnvoyGeneratedFailureBaseline',
}
terminal = {}
skipped = []
with open(sys.argv[1]) as stream:
    for line in stream:
        event = json.loads(line)
        name = event.get('Test')
        if event.get('Action') == 'skip':
            skipped.append(name)
        if name in expected and event.get('Action') in {'pass', 'fail', 'skip'}:
            terminal[name] = event['Action']
if skipped or terminal != dict.fromkeys(expected, 'pass'):
    raise SystemExit('Real Envoy tests missing, skipped or failed: ' + repr({'terminal': terminal, 'skipped': skipped}))
print('All required real Envoy contract tests executed and passed.')
PY
