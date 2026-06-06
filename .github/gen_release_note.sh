#!/usr/bin/env bash
# Generate categorized release notes from conventional commit messages.
# Usage: ./gen_release_note.sh -v v0.1.0...v0.2.0
#        ./gen_release_note.sh -v "...v0.1.0"   (first release, no previous tag)
#
# Writes release.md. Sections: What's Changed, Bug Fixes, Maintenance.

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

# Parse prev and curr from the range string (split on literal "...")
# IFS treats each char as a separate delimiter, so we can't use IFS='...'
# instead we do explicit substring splitting.
if [[ "$version_range" == *"..."* ]]; then
  prev="${version_range%...*}"
  curr="${version_range#*...}"
elif [[ "$version_range" == "..."* ]]; then
  prev=""
  curr="${version_range:3}"
else
  prev=""
  curr="$version_range"
fi

REPO="https://github.com/${GITHUB_REPOSITORY:-tresor/tresor}"

# For first release (no prev), use the tag directly instead of A...B range.
# git log <single-tag> lists all commits reachable from that tag.
if [ -z "$prev" ]; then
  LOG_RANGE="$curr"
else
  LOG_RANGE="${prev}...${curr}"
fi

{
  echo "# What's New"
  echo ""

  # ── Features ──────────────────────────────────────────────────────
  echo "## :rocket: What's Changed"
  echo ""
  git log --pretty="* %h %s by @%an" --grep="^feat" -i "$LOG_RANGE" | sort -f | uniq
  echo ""

  # ── Bug fixes ─────────────────────────────────────────────────────
  echo "## :bug: Bug Fixes"
  echo ""
  git log --pretty="* %h %s by @%an" --grep="^fix" -i "$LOG_RANGE" | sort -f | uniq
  echo ""

  # ── Maintenance ───────────────────────────────────────────────────
  echo "## :wrench: Maintenance"
  echo ""
  git log --pretty="* %h %s by @%an" --grep="^ci\|^chore\|^docs\|^refactor\|^test" -i "$LOG_RANGE" | sort -f | uniq
  echo ""

  # ── Footer ────────────────────────────────────────────────────────
  if [ -n "$prev" ]; then
    echo "**Full Changelog**: ${REPO}/compare/${prev}...${curr}"
  else
    echo "**Full Changelog**: ${REPO}/releases/tag/${curr}"
  fi
} > release.md
