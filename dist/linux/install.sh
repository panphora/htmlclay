#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# The tarball puts the binary next to this script; a repo checkout leaves it at
# the root, where dist/linux/build.sh writes it. Accepting both makes this the
# one install path, instead of a tarball-only script shadowed by a list of manual
# steps printed elsewhere that drifts away from it.
if [ -x "$SCRIPT_DIR/htmlclay" ]; then
  BINARY="$SCRIPT_DIR/htmlclay"
elif [ -x "$SCRIPT_DIR/../../htmlclay" ]; then
  BINARY="$(cd "$SCRIPT_DIR/../.." && pwd)/htmlclay"
else
  echo "install.sh: no htmlclay binary next to this script or at the repo root." >&2
  echo "            Unpack the release tarball and run install.sh from it, or build one with 'make dist-linux'." >&2
  exit 1
fi

# ~/.local/bin, and no sudo. It is on PATH on every current desktop distribution
# and it is where the freedesktop base directory spec puts a single user's own
# programs. HTML Clay runs as one user, keeps its state under that user's config
# directory, and registers its file type for that user alone, so asking for the
# root password to put the binary somewhere every account can see it was asking
# for more than the program ever uses.
#
# --system, or BIN_DIR=/usr/local/bin, still installs the old way.
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}"

for arg in "$@"; do
  case "$arg" in
    --system) BIN_DIR=/usr/local/bin ;;
    -h|--help)
      echo "usage: install.sh [--system]"
      echo "  --system      install into /usr/local/bin (uses sudo)"
      echo "  BIN_DIR=<dir> install into <dir> instead"
      exit 0
      ;;
    *) echo "install.sh: unknown option $arg (try --help)" >&2; exit 2 ;;
  esac
done

# sudo is decided by whether the destination is writable, not by what it is
# called, so a BIN_DIR the user owns never prompts and one they do not always
# does, whichever path they named.
mkdir -p "$BIN_DIR" 2>/dev/null || true
if [ -w "$BIN_DIR" ]; then
  install -m 755 "$BINARY" "$BIN_DIR/htmlclay"
else
  echo "Installing htmlclay to $BIN_DIR (may prompt for sudo)..."
  sudo mkdir -p "$BIN_DIR"
  sudo install -m 755 "$BINARY" "$BIN_DIR/htmlclay"
fi

mkdir -p "$DATA_DIR/applications" "$DATA_DIR/mime/packages"

# The shipped .desktop carries a bare Exec=htmlclay, resolved off PATH. A desktop
# session builds its PATH from the login shell's environment, not from the
# terminal the install ran in, so a fresh ~/.local/bin that this shell can see
# may be invisible to the session that launches the icon. Rewriting Exec to the
# path we just installed to removes the lookup entirely. Everything else in the
# file is copied through untouched.
{
  grep -v '^Exec=' htmlclay.desktop
  printf 'Exec="%s" %%f\n' "$BIN_DIR/htmlclay"
} > "$DATA_DIR/applications/htmlclay.desktop"

cp htmlclay-mime.xml "$DATA_DIR/mime/packages/"

bash "$SCRIPT_DIR/install-icon.sh"

update-mime-database "$DATA_DIR/mime" 2>/dev/null || true
update-desktop-database "$DATA_DIR/applications" 2>/dev/null || true

echo ""
echo "Installed. Double-click a .htmlclay file, or run: $BIN_DIR/htmlclay yourfile.htmlclay"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "Note: $BIN_DIR is not on your PATH, so 'htmlclay' will not work as a bare command yet." ;;
esac

if ! command -v zenity >/dev/null 2>&1 && ! command -v kdialog >/dev/null 2>&1; then
  echo "Note: install zenity or kdialog to get HTML Clay's permission prompts. Without one,"
  echo "      a file opened from outside a trusted folder is refused rather than asked about."
fi

echo "To remove it again: bash \"$SCRIPT_DIR/uninstall.sh\""
