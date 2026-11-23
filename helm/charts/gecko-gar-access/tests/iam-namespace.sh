#!/usr/bin/env bash
set -euo pipefail

chart_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

for namespace in 123 0123 true false null 1e3 gecko-system; do
	helm template test "$chart_dir" --namespace gecko-system \
		--set image.tag=test \
		--set-string garAccess.repository=projects/platform-project/locations/us-central1/repositories/gcp-hcp-quay \
		--set-string "garAccess.iamNamespace=$namespace" \
		--show-only templates/rbac.yaml > "$tmp_dir/rbac.yaml"
	if [[ $(grep -c "^  namespace: \"$namespace\"$" "$tmp_dir/rbac.yaml") != 2 ]]; then
		echo "IAM namespace was not rendered as a string in both RBAC resources: $namespace" >&2
		exit 1
	fi
done

echo 'IAM namespace chart tests passed.'
