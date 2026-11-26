#!/usr/bin/env bash
# Validates commit messages against the Conventional Commits spec.
# https://www.conventionalcommits.org/en/v1.0.0/
#
# Called by the commit-msg pre-commit hook with the commit message file as $1.

set -e

commit_msg=$(cat "$1")

# Skip merge commits and revert auto-messages
if echo "$commit_msg" | grep -qE "^Merge |^Merged |^Revert \""; then
  exit 0
fi

# Skip fixup/squash commits (used during interactive rebase)
if echo "$commit_msg" | grep -qE "^(fixup|squash)!"; then
  exit 0
fi

# type(scope)!: description — scope and breaking-change marker are optional
pattern="^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\([^)]+\))?(!)?: .+"

if ! echo "$commit_msg" | grep -qE "$pattern"; then
  echo ""
  echo "  Commit message does not follow Conventional Commits spec."
  echo "  https://www.conventionalcommits.org/en/v1.0.0/"
  echo ""
  echo "  Format:  <type>(<scope>): <description>   (scope optional)"
  echo "  Types:   feat | fix | docs | style | refactor | perf | test | build | ci | chore | revert"
  echo "  Example: feat: add restore-time histogram"
  echo "           fix(chart): grant watch on applications"
  echo "           chore(deps): update golangci-lint"
  echo ""
  echo "  Got: $commit_msg"
  echo ""
  exit 1
fi
