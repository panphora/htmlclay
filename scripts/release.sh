#!/bin/bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

# ── Colors ──
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
DIM='\033[2m'
RESET='\033[0m'

# ── Helpers ──
log()     { echo -e "$@"; }
info()    { log "${BLUE}→ $1${RESET}"; }
success() { log "${GREEN}✓ $1${RESET}"; }
warn()    { log "${YELLOW}⚠ $1${RESET}"; }
error()   { log "${RED}✗ $1${RESET}"; }
section() { log "\n${CYAN}══════════════════════════════════════════════════${RESET}"; log "${CYAN}  $1${RESET}"; log "${CYAN}══════════════════════════════════════════════════${RESET}\n"; }

# ── Parse args ──
BUMP_TYPE=""
RESUME=false
for arg in "$@"; do
  case "$arg" in
    --major) BUMP_TYPE="major" ;;
    --minor) BUMP_TYPE="minor" ;;
    --patch) BUMP_TYPE="patch" ;;
    --resume) RESUME=true ;;
    --help|-h)
      echo "Usage: ./scripts/release.sh [--major|--minor|--patch]"
      echo "       ./scripts/release.sh --resume"
      echo ""
      echo "  --major    Major version bump (breaking changes)"
      echo "  --minor    Minor version bump (new features)"
      echo "  --patch    Patch version bump (bug fixes)"
      echo "  --resume   Finish the release already in the source, without bumping"
      echo ""
      echo "If no option is provided, defaults to --patch."
      exit 0
      ;;
    *) error "Unknown argument: $arg"; exit 1 ;;
  esac
done

if [ "$RESUME" = true ] && [ -n "$BUMP_TYPE" ]; then
  error "--resume finishes the version already in the source, so it cannot take a bump."
  exit 1
fi

if [ -z "$BUMP_TYPE" ] && [ "$RESUME" != true ]; then
  BUMP_TYPE="patch"
  info "No bump type specified, defaulting to --patch"
fi

START_TIME=$(date +%s)

# ══════════════════════════════════════════════════
section "Step 1: Pre-flight Checks"
# ══════════════════════════════════════════════════

# Check required tools
for tool in go gh; do
  if ! command -v "$tool" &>/dev/null; then
    error "Required tool not found: $tool"
    exit 1
  fi
done
success "Required tools available"

# Check for uncommitted changes
if [ -n "$(git status --porcelain)" ]; then
  error "Uncommitted changes detected:"
  git status --short
  echo ""
  echo "Please commit or stash changes before releasing."
  exit 1
fi
success "Working directory clean"

# Check gh auth
if ! gh auth status &>/dev/null; then
  error "Not authenticated with GitHub CLI. Run: gh auth login"
  exit 1
fi
success "GitHub CLI authenticated"

# ══════════════════════════════════════════════════
section "Step 2: Version Bump"
# ══════════════════════════════════════════════════

CURRENT_VERSION=$(grep 'var version' cmd/htmlclay/main.go | sed 's/.*"\(.*\)"/\1/')
log "Current version: ${CURRENT_VERSION}"

# The tag is this script's completion marker: step 4 creates it only once CI is
# green for the exact commit. So a source version with no tag means a release of
# it was STARTED and never finished, and bumping again would ship the next
# number and skip that one forever. That is precisely what "re-run with the same
# version" asked for below and, until --resume existed, had no way to do.
#
# Tags are fetched first because a fresh clone has none, and "no tag" would then
# describe every version.
git fetch --tags --quiet origin 2>/dev/null || true
UNFINISHED=false
if ! git rev-parse -q --verify "refs/tags/v${CURRENT_VERSION}^{commit}" >/dev/null 2>&1; then
  UNFINISHED=true
fi

if [ "$UNFINISHED" = true ] && [ "$RESUME" != true ]; then
  error "v${CURRENT_VERSION} is in the source but was never tagged."
  error "A release of it was started and did not finish, so bumping now would ship a"
  error "new number and skip v${CURRENT_VERSION} for good."
  echo ""
  echo "  Finish it:        ./scripts/release.sh --resume"
  echo "  Check what shipped: curl -s https://download.htmlclay.com/htmlclay-release-info.json"
  echo ""
  echo "If the artifacts really did publish and only the website stamp is missing, the"
  echo "tag exists and you will not see this; re-run the stamp instead."
  exit 1
fi

if [ "$RESUME" = true ]; then
  if [ "$UNFINISHED" != true ]; then
    error "Nothing to resume: v${CURRENT_VERSION} is already tagged."
    error "Run with --major/--minor/--patch to release something new."
    exit 1
  fi
  NEW_VERSION="$CURRENT_VERSION"
  success "Resuming the unfinished release of v${NEW_VERSION}"
else
  IFS='.' read -r MAJOR MINOR PATCH <<< "$CURRENT_VERSION"
  case "$BUMP_TYPE" in
    major) NEW_VERSION="$((MAJOR + 1)).0.0" ;;
    minor) NEW_VERSION="${MAJOR}.$((MINOR + 1)).0" ;;
    patch) NEW_VERSION="${MAJOR}.${MINOR}.$((PATCH + 1))" ;;
  esac

  success "Version: ${CURRENT_VERSION} → ${NEW_VERSION} (${BUMP_TYPE})"

  # Update version in cmd/htmlclay/main.go
  sed -i '' "s/var version = \"${CURRENT_VERSION}\"/var version = \"${NEW_VERSION}\"/" cmd/htmlclay/main.go
  success "Updated cmd/htmlclay/main.go"
fi

# The website is stamped after CI, not here. Pushing to main auto-deploys the
# site, so stamping now would advertise downloads minutes before they exist.

# ══════════════════════════════════════════════════
section "Step 3: Commit & Push"
# ══════════════════════════════════════════════════

# A resume has nothing to bump, so it has nothing to commit: the release commit
# is already in the tree, which is why the preflight's clean-tree check passed.
# `git commit` with an empty index exits non-zero and `set -e` would take the
# whole script down at the one moment it exists to recover from.
if [ "$RESUME" = true ]; then
  info "Version already committed by the run being resumed"
else
  git add cmd/htmlclay/main.go website/index.html
  git commit -m "chore: release v${NEW_VERSION}"
  success "Committed version bump"
fi

# Captured BEFORE the push, and pushed BY SHA rather than by HEAD. This working
# tree is shared with other sessions and with ferry, which auto-commits any idle
# repo outside Tue-Fri 09:00-18:00 — the exact window a release runs in. Pushing
# HEAD and then reading `git rev-parse HEAD` would record, build, and tag
# whatever landed in between instead of the release commit.
RELEASE_SHA="$(git rev-parse HEAD)"
BRANCH="$(git branch --show-current)"
info "Pushing ${RELEASE_SHA:0:7} to ${BRANCH}..."
git push origin "${RELEASE_SHA}:refs/heads/${BRANCH}"
success "Pushed commit ${RELEASE_SHA:0:7}"

# The tag is created in step 4, AFTER the gate passes. Tagging here published a tag
# for a release nothing had tested yet, so a red gate left a permanent tag claiming a
# version that never shipped. That is what happened on 2026-07-27: the run died at the
# Windows test gate, was never retried, and users sat on 1.1.1 for four weeks while
# main carried two shipped features.

# ══════════════════════════════════════════════════
section "Step 4: Build & Publish via CI"
# ══════════════════════════════════════════════════

info "Triggering release workflow on GitHub Actions..."
# source_sha pins every job's checkout to the commit just pushed, so a push
# landing between here and the runner starting cannot ship code the test gate
# never saw. It also lands in the manifest, which is what lets the board tell
# "this build is the tree" from "this build is older than the tree".
# --ref is explicit: without it `gh workflow run` dispatches on the remote
# DEFAULT branch, which is only incidentally the one being released.
gh workflow run release.yml --ref "${BRANCH}" \
  -f version="${NEW_VERSION}" -f source_sha="${RELEASE_SHA}"

info "Waiting for workflow run to appear..."
RUN_ID=""
for _ in $(seq 1 15); do
  sleep 3
  # Match the exact release SHA. `--limit 1` alone takes the newest dispatch, which can
  # belong to a different commit, and watching a green run for someone else's commit
  # would tag and ship an untested one.
  #
  # The NAME is the primary key, not headSha. release.yml's `run-name` embeds
  # source_sha, whereas a workflow_dispatch run's headSha is the head of the ref
  # at dispatch time: a commit landing between the push and the dispatch makes
  # headSha != RELEASE_SHA while every job still correctly builds RELEASE_SHA.
  # The headSha arm stays as a fallback for runs dispatched before run-name existed.
  RUN_ID=$(gh run list --workflow=release.yml --event workflow_dispatch \
    --limit 20 --json databaseId,displayTitle,headSha \
    -q "[.[] | select((.displayTitle | contains(\"${RELEASE_SHA}\")) or .headSha == \"${RELEASE_SHA}\")] | .[0].databaseId" 2>/dev/null || echo "")
  [ "$RUN_ID" = "null" ] && RUN_ID=""
  [ -n "$RUN_ID" ] && break
done

if [ -z "$RUN_ID" ]; then
  error "No release run found for ${RELEASE_SHA:0:7} — refusing to tag."
  error "The version commit is pushed. Check GitHub Actions, then: ./scripts/release.sh --resume"
  exit 1
fi

info "Watching run ${RUN_ID}..."
gh run watch "$RUN_ID" --exit-status || {
  error "CI workflow failed! Check: gh run view $RUN_ID"
  error "No tag was created, so nothing claims a release. Fix forward on main, then:"
  error "  ./scripts/release.sh --resume"
  exit 1
}
success "CI workflow completed"

# Green for this exact commit, so the tag can now claim a release that really exists.
# Tag RELEASE_SHA explicitly: six minutes have passed since it was captured, and a
# bare `git tag` names current HEAD, so anything committed meanwhile (ferry, another
# session) would be tagged as the release CI never built.
info "Tagging v${NEW_VERSION}..."
git tag "v${NEW_VERSION}" "${RELEASE_SHA}"
git push origin "v${NEW_VERSION}"
success "Tagged and pushed v${NEW_VERSION}"

# ══════════════════════════════════════════════════
section "Step 5: Publish Website"
# ══════════════════════════════════════════════════

# Pushing to main auto-deploys the site through the Cloudflare Workers git
# integration, so publishing the site means pushing the stamped page. This runs
# after CI so the links never point at artifacts that are not on R2 yet.
info "Stamping website with v${NEW_VERSION}..."
CHECKSUM_FILE="$(mktemp "${TMPDIR:-/tmp}/htmlclay-checksums.XXXXXX")"
trap 'rm -f "$CHECKSUM_FILE"' EXIT
# R2 can lag a little behind the workflow finishing, and a release that dies here
# would leave the artifacts published and the site unstamped, so give it a few tries
# before giving up. The query is a cache-buster: without it a CDN copy of the previous
# release's SHA256SUMS can be served and stamped, which is worse than failing. It
# varies per ATTEMPT, not just per version, because Cloudflare caches 404s too: one
# constant key means attempts 2 to 5 are served the negative response the first
# attempt just cached, and a re-run of the same version reuses the failed run's key.
fetched=false
for attempt in 1 2 3 4 5; do
  if curl -fsS "https://download.htmlclay.com/SHA256SUMS?v=${NEW_VERSION}-${attempt}" -o "$CHECKSUM_FILE"; then
    fetched=true
    break
  fi
  warn "SHA256SUMS not readable yet (attempt ${attempt}/5); waiting 10s..."
  sleep 10
done
if [ "$fetched" != true ]; then
  error "Could not fetch SHA256SUMS after 5 attempts. The artifacts are published; re-run the website stamp once R2 has it."
  exit 1
fi
node scripts/stamp-website.js "${NEW_VERSION}" "$CHECKSUM_FILE"
rm -f "$CHECKSUM_FILE"
trap - EXIT

# Keep this list in step with PAGES in scripts/stamp-website.js: a page stamped
# and not staged ships stale and leaves the tree dirty, which fails the next
# release's clean-tree check.
git add website/index.html
if git diff --cached --quiet; then
  info "Website already current, nothing to push"
else
  git commit -m "chore: point downloads at v${NEW_VERSION}"
  git push origin HEAD
  success "Pushed stamped website"

  info "Waiting for the auto-deploy to go live..."
  # Cache-busted: the edge serves a cached root document for minutes after a
  # deploy, which otherwise reads as a failed deploy here.
  LIVE=false
  for i in $(seq 1 24); do
    sleep 5
    if curl -s -H 'Cache-Control: no-cache' "https://htmlclay.com/?rc=${NEW_VERSION}-${i}" \
      | grep -q "HTMLClay-${NEW_VERSION}-universal.dmg"; then
      LIVE=true
      break
    fi
  done
  if [ "$LIVE" = true ]; then
    success "htmlclay.com is serving v${NEW_VERSION}"
  else
    warn "htmlclay.com has not picked up v${NEW_VERSION} yet. Check the Cloudflare"
    warn "Workers build for panphora/htmlclay; the page is pushed and will deploy."
  fi
fi

# ══════════════════════════════════════════════════
section "Step 6: Install Locally"
# ══════════════════════════════════════════════════

bash scripts/install-local.sh "${NEW_VERSION}" \
  || warn "Could not install locally — download it from https://htmlclay.com/#downloads"

# ══════════════════════════════════════════════════
section "Step 7: Done"
# ══════════════════════════════════════════════════

END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))
MINUTES=$((DURATION / 60))
SECONDS=$((DURATION % 60))

log "Version:  ${NEW_VERSION}"
log "Duration: ${MINUTES}m ${SECONDS}s"
log ""
log "All platforms (macOS universal, Linux, Windows): R2 via CI"
log ""
log "Git tag:  v${NEW_VERSION}"
log "Download: https://htmlclay.com/#downloads"
log ""
success "Release v${NEW_VERSION} complete!"
