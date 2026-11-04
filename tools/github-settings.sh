#!/usr/bin/env bash
# Repository settings on GitHub for a public repository. A change reaches main only by a pull
# request that passes CI ("ci-ok") and is approved by a code owner (.github/CODEOWNERS); the
# repository admin (the owner) may push directly. Everybody without write access works in a fork.
# Workflows from a fork's pull request run only after the owner approved them. Security: Dependabot
# alerts and fixes, secret scanning with push protection, private vulnerability reports.
# Rulesets, the fork approval and secret scanning need a public repository (or a paid plan), so
# run this right after making the repository public. Running it again changes nothing.
#
#   tools/github-settings.sh owner/repo
set -euo pipefail

repo="${1:?usage: $0 owner/repo}"
name="main: pull request, CI and review"

# A pull request's branch deleted after the merge, no auto-merge, no wiki (docs live in the
# repository). Forks need no setting: a public repository can always be forked.
gh api -X PATCH "repos/$repo" \
  -F delete_branch_on_merge=true -F allow_auto_merge=false -F has_wiki=false >/dev/null

# The default branch: no deletion, no force push; changes by pull request with "ci-ok" passed
# (the check of the GitHub Actions app, 15368) and one code owner's approval, given again after a
# new push. The admin role (5) bypasses the rules: the owner's own pushes and merges.
body=$(mktemp)
trap 'rm -f "$body"' EXIT
cat >"$body" <<JSON
{
  "name": "$name",
  "target": "branch",
  "enforcement": "active",
  "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []}},
  "bypass_actors": [{"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always"}],
  "rules": [
    {"type": "deletion"},
    {"type": "non_fast_forward"},
    {"type": "pull_request", "parameters": {
      "required_approving_review_count": 1,
      "require_code_owner_review": true,
      "dismiss_stale_reviews_on_push": true,
      "require_last_push_approval": true,
      "required_review_thread_resolution": false}},
    {"type": "required_status_checks", "parameters": {
      "strict_required_status_checks_policy": false,
      "required_status_checks": [{"context": "ci-ok", "integration_id": 15368}]}}
  ]
}
JSON
for old in "default branch: owner only" "$name"; do
  id=$(gh api "repos/$repo/rulesets" --jq ".[] | select(.name == \"$old\") | .id")
  [ -z "$id" ] || gh api -X DELETE "repos/$repo/rulesets/$id" >/dev/null
done
gh api -X POST "repos/$repo/rulesets" --input "$body" >/dev/null

# Actions: a workflow from a fork's pull request waits for the owner's approval, for every
# outside contributor (not only first-time ones); the workflow token is read-only and may not
# approve pull requests.
gh api -X PUT "repos/$repo/actions/permissions/fork-pr-contributor-approval" \
  -f approval_policy=all_external_contributors >/dev/null
gh api -X PUT "repos/$repo/actions/permissions/workflow" \
  -f default_workflow_permissions=read -F can_approve_pull_request_reviews=false >/dev/null

# Security: Dependabot alerts and security updates, secret scanning with push protection, and
# private vulnerability reports (SECURITY.md).
gh api -X PUT "repos/$repo/vulnerability-alerts" >/dev/null
gh api -X PUT "repos/$repo/automated-security-fixes" >/dev/null
gh api -X PUT "repos/$repo/private-vulnerability-reporting" >/dev/null
gh api -X PATCH "repos/$repo" --input - >/dev/null <<JSON
{"security_and_analysis": {"secret_scanning": {"status": "enabled"},
                           "secret_scanning_push_protection": {"status": "enabled"}}}
JSON

echo "settings applied to $repo"
gh api "repos/$repo/collaborators" --jq '"collaborators with access: " + ([.[].login] | join(", "))'
