#!/usr/bin/env bash
set -euo pipefail

chart_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

render() {
	helm template test "$chart_dir" --set image.tag=test \
		--set-string garAccess.repository=projects/platform-project/locations/us-central1/repositories/gcp-hcp-quay \
		--show-only templates/deployment.yaml "$@" > "$tmp_dir/deployment.yaml"
}

render
grep -q '^            - --workers=10$' "$tmp_dir/deployment.yaml"
if grep -q -- '--orlop-url=' "$tmp_dir/deployment.yaml"; then
	echo 'Default configuration must use the in-cluster Gecko API' >&2
	exit 1
fi

render --set workers=3 --set-string orlopURL=https://gecko.example.test:8443
grep -q '^            - --workers=3$' "$tmp_dir/deployment.yaml"
grep -Fxq '            - --orlop-url=https://gecko.example.test:8443' "$tmp_dir/deployment.yaml"
[[ $(grep -c -- '--workers=' "$tmp_dir/deployment.yaml") == 1 ]]
[[ $(grep -c -- '--orlop-url=' "$tmp_dir/deployment.yaml") == 1 ]]

echo 'GAR controller settings chart tests passed.'
