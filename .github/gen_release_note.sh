#!/usr/bin/env bash
# Generate categorized release notes from conventional commit messages.
# Usage: ./gen_release_note.sh -v v0.1.0...v0.2.0
#
# Writes release.md to stdout. Sections: What's Changed, Bug Fixes, Maintenance.

set -euo pipefail

version_range=""

while getopts "v:" opt; do
  case $opt in
    v) version_range=$OPTARG ;;
    \?) echo "Usage: $0 -v <prev_tag>...<curr_tag>" >&2; exit 1 ;;
  esac
done

if [ -z "$version_range" ]; then
  echo "Error: --version (-v) is required." >&2
  echo "Example: $0 -v v0.1.0...v0.2.0" >&2
  exit 1
fi

REPO="https://github.com/${GITHUB_REPOSITORY}"

{
  echo "# What's New"
  echo ""

  # ── Features ──────────────────────────────────────────────────────
  echo "## :rocket: What's Changed"
  echo ""
  git log --pretty="* %h %s by @%an" --grep="^feat" -i "$version_range" | sort -f | uniq
  echo ""

  # ── Bug fixes ─────────────────────────────────────────────────────
  echo "## :bug: Bug Fixes"
  echo ""
  git log --pretty="* %h %s by @%an" --grep="^fix" -i "$version_range" | sort -f | uniq
  echo ""

  # ── Maintenance ───────────────────────────────────────────────────
  echo "## :wrench: Maintenance"
  echo ""
  git log --pretty="* %h %s by @%an" --grep="^ci\|^chore\|^docs\|^refactor\|^test" -i "$version_range" | sort -f | uniq
  echo ""

  # ── Footer ────────────────────────────────────────────────────────
  IFS='...' read -r prev curr <<< "$version_range"
  echo "**Full Changelog**: ${REPO}/compare/${prev}...${curr}"
} > release.md
