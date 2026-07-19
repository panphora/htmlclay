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
for arg in "$@"; do
  case "$arg" in
    --major) BUMP_TYPE="major" ;;
    --minor) BUMP_TYPE="minor" ;;
    --patch) BUMP_TYPE="patch" ;;
    --help|-h)
      echo "Usage: ./scripts/release.sh [--major|--minor|--patch]"
      echo ""
      echo "  --major    Major version bump (breaking changes)"
      echo "  --minor    Minor version bump (new features)"
      echo "  --patch    Patch version bump (bug fixes)"
      echo ""
      echo "If no option is provided, defaults to --patch."
      exit 0
      ;;
    *) error "Unknown argument: $arg"; exit 1 ;;
  esac
done

if [ -z "$BUMP_TYPE" ]; then
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

CURRENT_VERSION=$(grep 'var version' main.go | sed 's/.*"\(.*\)"/\1/')
log "Current version: ${CURRENT_VERSION}"

IFS='.' read -r MAJOR MINOR PATCH <<< "$CURRENT_VERSION"
case "$BUMP_TYPE" in
  major) NEW_VERSION="$((MAJOR + 1)).0.0" ;;
  minor) NEW_VERSION="${MAJOR}.$((MINOR + 1)).0" ;;
  patch) NEW_VERSION="${MAJOR}.${MINOR}.$((PATCH + 1))" ;;
esac

success "Version: ${CURRENT_VERSION} → ${NEW_VERSION} (${BUMP_TYPE})"

# Update version in main.go
sed -i '' "s/var version = \"${CURRENT_VERSION}\"/var version = \"${NEW_VERSION}\"/" main.go
success "Updated main.go"

# The website is stamped after CI, not here. Pushing to main auto-deploys the
# site, so stamping now would advertise downloads minutes before they exist.

# ══════════════════════════════════════════════════
section "Step 3: Commit, Tag & Push"
# ══════════════════════════════════════════════════

git add main.go website/index.html
git commit -m "chore: release v${NEW_VERSION}"
success "Committed version bump"

git tag "v${NEW_VERSION}"
success "Tagged v${NEW_VERSION}"

info "Pushing to remote..."
git push origin HEAD
git push origin "v${NEW_VERSION}"
success "Pushed commit and tag"

# ══════════════════════════════════════════════════
section "Step 4: Build & Publish via CI"
# ══════════════════════════════════════════════════

info "Triggering release workflow on GitHub Actions..."
BRANCH="$(git branch --show-current)"
gh workflow run release.yml -f version="${NEW_VERSION}"

info "Waiting for workflow run to appear..."
RUN_ID=""
for _ in $(seq 1 15); do
  sleep 3
  RUN_ID=$(gh run list --workflow=release.yml --branch "$BRANCH" --event workflow_dispatch \
    --limit 1 --json databaseId,status -q '.[0].databaseId' 2>/dev/null || echo "")
  [ -n "$RUN_ID" ] && break
done

if [ -n "$RUN_ID" ]; then
  info "Watching run ${RUN_ID}..."
  gh run watch "$RUN_ID" --exit-status || {
    error "CI workflow failed! Check: gh run view $RUN_ID"
    exit 1
  }
  success "CI workflow completed"
else
  warn "Could not find CI run — check GitHub Actions manually"
fi

# ══════════════════════════════════════════════════
section "Step 5: Publish Website"
# ══════════════════════════════════════════════════

# Pushing to main auto-deploys the site through the Cloudflare Workers git
# integration, so publishing the site means pushing the stamped page. This runs
# after CI so the links never point at artifacts that are not on R2 yet.
info "Stamping website with v${NEW_VERSION}..."
node scripts/stamp-website.js "${NEW_VERSION}"

git add website/index.html
if git diff --cached --quiet; then
  info "Website already current, nothing to push"
else
  git commit -m "chore: point downloads at v${NEW_VERSION}"
  git push origin HEAD
  success "Pushed stamped website"

  info "Waiting for the auto-deploy to go live..."
  LIVE=false
  for _ in $(seq 1 18); do
    sleep 5
    if curl -s https://htmlclay.com/ | grep -q "HTMLClay-${NEW_VERSION}-universal.dmg"; then
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
