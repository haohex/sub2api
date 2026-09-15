#!/usr/bin/env bash
# Run the trusted copy from RUNNER_TEMP, never a helper from the sync branch.
set -euo pipefail

base_ref="refs/remotes/origin/${BASE_BRANCH:?}"
if git merge-base --is-ancestor "$base_ref" HEAD; then
  exit 0
fi

git config user.name 'github-actions[bot]'
git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
if git -c core.hooksPath=/dev/null merge --no-edit --no-ff "$base_ref" \
  -m 'ci(上游同步): 自动合入主分支' -m 'Refs #13'; then
  exit 0
fi

if [ -z "$(git ls-files --unmerged)" ]; then
  echo '::error::Failed to merge main for a reason other than conflicts'
  exit 1
fi
git -c core.hooksPath=/dev/null merge --abort
echo '::warning::main has conflicts with the sync branch; resolve them manually in the review PR'
