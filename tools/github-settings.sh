#!/usr/bin/env bash
# Repository settings on GitHub for a public repository: only the owner (admin) pushes to the
# default branch; everybody else works in a fork and opens a pull request, and workflows from a
# fork's pull request run only after the owner approved them. Rulesets and the fork approval need
# a public repository (or GitHub Pro), so run this right after making the repository public.
# Running it again replaces the ruleset with the same content.
#
#   tools/github-settings.sh owner/repo
set -euo pipefail

repo="${1:?usage: $0 owner/repo}"
name="default branch: owner only"

# A pull request's branch deleted after the merge, no auto-merge, no wiki (docs live in the
# repository). Forks need no setting: a public repository can always be forked.
gh api -X PATCH "repos/$repo" \
  -F delete_branch_on_merge=true -F allow_auto_merge=false -F has_wiki=false >/dev/null

# The default branch: no deletion, no force push, no update except by the repository admin.
body=$(mktemp)
trap 'rm -f "$body"' EXIT
cat >"$body" <<JSON
{
  "name": "$name",
  "target": "branch",
  "enforcement": "active",
  "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []}},
  "bypass_actors": [{"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always"}],
  "rules": [{"type": "deletion"}, {"type": "non_fast_forward"}, {"type": "update"}]
}
JSON
id=$(gh api "repos/$repo/rulesets" --jq ".[] | select(.name == \"$name\") | .id")
if [ -n "$id" ]; then
  gh api -X PUT "repos/$repo/rulesets/$id" --input "$body" >/dev/null
else
  gh api -X POST "repos/$repo/rulesets" --input "$body" >/dev/null
fi

# Actions: a workflow from a fork's pull request waits for the owner's approval, for every
# outside contributor (not only first-time ones); the workflow token is read-only and may not
# approve pull requests.
gh api -X PUT "repos/$repo/actions/permissions/fork-pr-contributor-approval" \
  -f approval_policy=all_external_contributors >/dev/null
gh api -X PUT "repos/$repo/actions/permissions/workflow" \
  -f default_workflow_permissions=read -F can_approve_pull_request_reviews=false >/dev/null

echo "settings applied to $repo"
gh api "repos/$repo/collaborators" --jq '"collaborators with access: " + ([.[].login] | join(", "))'
