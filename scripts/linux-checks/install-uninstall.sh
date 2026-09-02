#!/bin/bash
# The tarball a Linux user downloads: unpack, install.sh into a fresh HOME, and the
# desktop must know the file type; then uninstall.sh leaves no trace but config.
# Mirrors release.yml's packaging step so the staged tree is the shipped tree.
source "$(dirname "$0")/lib.sh"
export XDG_CURRENT_DESKTOP=GNOME   # xdg-mime takes the gio path Ubuntu users are on

stage="$LAB/stage"; mkdir -p "$stage"
cp "$BIN" "$stage/htmlclay"
for f in htmlclay.desktop htmlclay-mime.xml htmlclay.png htmlclay.svg application-x-htmlclay.png application-x-htmlclay.svg install-icon.sh install.sh uninstall.sh; do
  cp "$REPO/packaging/linux/$f" "$stage/"
done
chmod +x "$stage"/*.sh
tar czf "$LAB/htmlclay-linux.tar.gz" -C "$stage" .
mkdir -p "$LAB/unpacked" && tar xzf "$LAB/htmlclay-linux.tar.gz" -C "$LAB/unpacked"
pass "tarball built and unpacked ($(du -h "$LAB/htmlclay-linux.tar.gz" | cut -f1))"

( cd "$HOME" && find . -type f | sort ) > "$LAB/files-before"
bash "$LAB/unpacked/install.sh" > "$LAB/install.log" 2>&1 || fail "install.sh exited $?: $(cat "$LAB/install.log")"
keep "$LAB/install.log"

test -x "$HOME/.local/bin/htmlclay" || fail "binary not installed to ~/.local/bin"
D="$XDG_DATA_HOME/applications/htmlclay.desktop"
test -f "$D" || fail "no .desktop installed"
grep -qxF "Exec=\"$HOME/.local/bin/htmlclay\" %f" "$D" || fail "Exec= not rewritten to the installed path: $(grep '^Exec=' "$D")"
if command -v desktop-file-validate >/dev/null; then
  desktop-file-validate "$D" || fail "desktop-file-validate rejected the entry"
  pass ".desktop validates"
fi
test -f "$XDG_DATA_HOME/mime/packages/htmlclay-mime.xml" || fail "MIME definition not installed"
for icon in icons/hicolor/256x256/apps/htmlclay.png icons/hicolor/scalable/apps/htmlclay.svg \
            icons/hicolor/256x256/mimetypes/application-x-htmlclay.png icons/hicolor/scalable/mimetypes/application-x-htmlclay.svg; do
  test -f "$XDG_DATA_HOME/$icon" || fail "icon missing: $icon"
done
pass "binary, .desktop, MIME xml and four icons in place"

echo '<!doctype html><html><body>sample</body></html>' > "$HOME/sample.htmlclay"
type="$(xdg-mime query filetype "$HOME/sample.htmlclay" 2>/dev/null || true)"
[ "$type" = "application/x-htmlclay" ] || fail "desktop does not recognise .htmlclay (got '$type')"
xdg-mime default htmlclay.desktop application/x-htmlclay
[ "$(xdg-mime query default application/x-htmlclay)" = "htmlclay.desktop" ] || fail "htmlclay.desktop is not the handler for .htmlclay"
pass "desktop maps .htmlclay -> application/x-htmlclay -> htmlclay.desktop"

"$HOME/.local/bin/htmlclay" wire help >/dev/null 2>&1 || fail "installed binary does not run"
pass "installed binary runs"

bash "$LAB/unpacked/uninstall.sh" > "$LAB/uninstall.log" 2>&1 || fail "uninstall.sh exited $?"
keep "$LAB/uninstall.log"
test ! -e "$HOME/.local/bin/htmlclay" || fail "binary survived uninstall"
test ! -e "$D" || fail ".desktop survived uninstall"
test ! -e "$XDG_DATA_HOME/mime/packages/htmlclay-mime.xml" || fail "MIME xml survived uninstall"
test ! -e "$XDG_DATA_HOME/icons/hicolor/256x256/apps/htmlclay.png" || fail "icon survived uninstall"
( cd "$HOME" && find . -type f | sort ) > "$LAB/files-after"
# What remains is the desktop's own caches (mime database, mimeapps.list) and our sample.
comm -13 "$LAB/files-before" "$LAB/files-after" | grep -v -E '^\./(sample\.htmlclay|\.config/mimeapps\.list|\.local/share/mime/|\.local/share/applications/mimeinfo\.cache|\.local/share/icons/hicolor/icon-theme\.cache)' > "$LAB/leftovers" || true
keep "$LAB/leftovers"
[ ! -s "$LAB/leftovers" ] || fail "uninstall left files behind: $(cat "$LAB/leftovers")"
pass "uninstall left nothing of ours behind"
