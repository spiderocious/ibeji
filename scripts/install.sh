#!/bin/sh
#
# Ìbejì installer.
#
#   curl -fsSL https://raw.githubusercontent.com/spiderocious/ibeji/main/scripts/install.sh | sh
#
# Downloads the right build for this Mac, installs it, starts the daemon and
# puts `ibeji` on PATH. One command, nothing to click, no terminal gymnastics
# afterwards.
#
# It also sidesteps Gatekeeper's "Apple could not verify..." prompt entirely —
# not by defeating it, but because that prompt comes from the quarantine flag
# a *browser* attaches to a download. curl does not set it, so an app fetched
# this way is never quarantined in the first place. Notarizing would be the
# other way, and it costs $99/yr.
#
# POSIX sh on purpose: this runs before anything of ours exists, on whatever
# shell the machine has.

set -eu

REPO="spiderocious/ibeji"
APP="/Applications/Ibeji.app"
# Printed in hints. $0 is "sh" when the script is piped, so it cannot be used.
SELF_URL="https://raw.githubusercontent.com/$REPO/main/scripts/install.sh"

# --- output -----------------------------------------------------------------

if [ -t 1 ]; then
  B=$(printf '\033[1m'); G=$(printf '\033[32m'); Y=$(printf '\033[33m')
  R=$(printf '\033[31m'); X=$(printf '\033[0m')
else
  B=''; G=''; Y=''; R=''; X=''
fi

FORCE=0
ASSUME_YES=0
for arg in "$@"; do
  case "$arg" in
    -f|--force) FORCE=1 ;;
    -y|--yes) ASSUME_YES=1 ;;
    -h|--help)
      cat <<'USAGE'
Install or update Ìbejì.

  curl -fsSL .../install.sh | sh
  curl -fsSL .../install.sh | sh -s -- --force

  -f, --force   Reinstall even when that version is already installed.
  -y, --yes     Do not ask anything; assume yes. For scripts and the app's
                own "Update now" button, which has already asked.
  -h, --help    This.
USAGE
      exit 0 ;;
    *) ;;
  esac
done

say()  { printf '%s\n' "$*"; }
step() { printf '%s==>%s %s\n' "$B" "$X" "$*"; }
ok()   { printf '  %s✓%s %s\n' "$G" "$X" "$*"; }
warn() { printf '  %s!%s %s\n' "$Y" "$X" "$*"; }
die()  { printf '  %s✗%s %s\n' "$R" "$X" "$*" >&2; exit 1; }

# --- checks -----------------------------------------------------------------

[ "$(uname -s)" = "Darwin" ] || die "Ìbejì is macOS only (this is $(uname -s))."

# Refuse root: the daemon must run in the user's login session to reach their
# keychain and GUI. Installing as root produces an app that cannot work, and
# the failure looks like a broken keychain rather than a misplaced install.
[ "$(id -u)" != "0" ] || die "Do not run this with sudo — Ìbejì installs into your own login session."

case "$(uname -m)" in
  arm64) ARCH="arm64"; LABEL="Apple Silicon" ;;
  x86_64) ARCH="x64";  LABEL="Intel" ;;
  *) die "Unsupported architecture: $(uname -m)" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required."

step "Ìbejì for $LABEL"

# --- find the release -------------------------------------------------------

# Resolve the current tag from the /releases/latest redirect rather than the
# JSON API. The API is rate-limited to 60 requests an hour per IP for
# unauthenticated callers — which an office, a NAT or anyone testing an
# installer will blow through, and the failure ("Could not reach GitHub")
# looks like an outage rather than a quota. The redirect has no such limit.
TAG=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
  "https://github.com/$REPO/releases/latest" 2>/dev/null | sed 's|.*/tag/||')

# Fall back to the API only if the redirect gave nothing useful.
if [ -z "${TAG:-}" ] || [ "$TAG" = "https://github.com/$REPO/releases/latest" ]; then
  TAG=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null \
    | awk -F'"' '/"tag_name"/ {print $4; exit}')
fi
[ -n "${TAG:-}" ] || die "Could not reach GitHub to find the latest release."
VERSION=${TAG#v}

if [ "$ARCH" = "arm64" ]; then
  DMG="Ibeji-$VERSION-arm64.dmg"
else
  DMG="Ibeji-$VERSION.dmg"
fi
URL="https://github.com/$REPO/releases/download/$TAG/$DMG"

ok "found $TAG"

# --- is this version already here? ------------------------------------------

# Without this the script happily spends 130MB and a service interruption to
# arrive exactly where it started: downloading, quitting the app, stopping the
# daemon, deleting the bundle, and writing back byte-identical files.
#
# Reinstalling is still the right move sometimes — a half-written bundle, a
# missing LaunchAgent, a deleted CLI link — so this asks rather than refuses.
INSTALLED=""
if [ -x "$APP/Contents/Resources/bin/ibeji" ]; then
  INSTALLED=$("$APP/Contents/Resources/bin/ibeji" --version 2>/dev/null | awk '{print $2}')
fi

if [ -n "$INSTALLED" ] && [ "$INSTALLED" = "$VERSION" ] && [ "$FORCE" -eq 0 ]; then
  say ""
  warn "Ìbejì $VERSION is already installed."
  say ""
  say "  Reinstalling downloads $DMG again, stops the daemon and replaces"
  say "  the app. Worth doing to repair a broken install; otherwise there is"
  say "  nothing to gain."
  say ""
  if [ "$ASSUME_YES" -eq 1 ]; then
    say "  Continuing anyway (--yes)."
    say ""
  elif [ -t 0 ]; then
    # A pipe into `sh` leaves stdin consumed by the script itself, so the
    # prompt has to read from the terminal directly or it gets EOF and
    # silently answers for the person.
    printf "  Reinstall anyway? [y/N] "
    read -r answer < /dev/tty || answer=""
    case "$answer" in
      [Yy]*) say "" ;;
      *)
        say ""
        say "  Nothing changed. Run with --force to reinstall without asking:"
        say "    curl -fsSL $SELF_URL | sh -s -- --force"
        say ""
        exit 0 ;;
    esac
  else
    # No terminal to ask on — a CI job or a nested script. Doing nothing is
    # the safe default; --force says otherwise.
    say "  No terminal to ask on, so nothing was changed."
    say "  Use --force to reinstall regardless."
    say ""
    exit 0
  fi
fi

if [ -n "$INSTALLED" ] && [ "$INSTALLED" != "$VERSION" ]; then
  step "Updating $INSTALLED → $VERSION"
fi

# --- download ---------------------------------------------------------------

TMP=$(mktemp -d)
# Clean up on any exit, including a failure partway through: a half-mounted
# image or a stray 130MB download is not something to leave behind.
cleanup() {
  [ -n "${MOUNT:-}" ] && [ -d "$MOUNT" ] && hdiutil detach "$MOUNT" -quiet 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

step "Downloading $DMG"
curl -fL --progress-bar -o "$TMP/$DMG" "$URL" || die "Download failed."

# Verify against the release's own checksums. A truncated download would
# otherwise install an app that fails in a way nobody could diagnose.
if curl -fsSL -o "$TMP/SHA256SUMS.txt" \
     "https://github.com/$REPO/releases/download/$TAG/SHA256SUMS.txt" 2>/dev/null; then
  WANT=$(awk -v f="$DMG" '$2 == f {print $1; exit}' "$TMP/SHA256SUMS.txt")
  if [ -n "${WANT:-}" ]; then
    GOT=$(shasum -a 256 "$TMP/$DMG" | awk '{print $1}')
    [ "$WANT" = "$GOT" ] || die "Checksum mismatch — the download is corrupt. Try again."
    ok "checksum verified"
  else
    warn "no checksum listed for $DMG; continuing"
  fi
else
  warn "could not fetch checksums; continuing"
fi

# --- install ----------------------------------------------------------------

step "Installing to /Applications"

MOUNT="$TMP/mnt"
mkdir -p "$MOUNT"
hdiutil attach -nobrowse -quiet "$TMP/$DMG" -mountpoint "$MOUNT" || die "Could not open the disk image."

# Quit a running copy first, or the replace below leaves a half-written bundle.
if pgrep -f "$APP/Contents/MacOS/Ibeji" >/dev/null 2>&1; then
  osascript -e 'tell application "Ibeji" to quit' >/dev/null 2>&1 || true
  sleep 2
  pkill -f "$APP/Contents/MacOS/Ibeji" 2>/dev/null || true
fi

# Stop the old daemon before swapping the binary it is running from.
if [ -x "$APP/Contents/Resources/bin/ibeji" ]; then
  "$APP/Contents/Resources/bin/ibeji" install --uninstall >/dev/null 2>&1 || true
fi

rm -rf "$APP"
cp -R "$MOUNT/Ibeji.app" /Applications/ || die "Could not write to /Applications."
hdiutil detach "$MOUNT" -quiet 2>/dev/null || true
MOUNT=""
ok "installed $APP"

# Belt and braces: curl does not quarantine, but a previous browser download
# may have left the flag on an older copy, and cp preserves what it finds.
xattr -dr com.apple.quarantine "$APP" 2>/dev/null || true

# --- set it up --------------------------------------------------------------

# Pick a bin directory this user can write, so nothing here needs sudo.
# /usr/local/bin is root-owned on a stock Mac; ~/.local/bin is the
# conventional answer and is created if absent. The prefix is passed
# explicitly rather than left to the CLI's default, so the script behaves the
# same against older releases.
PREFIX=""
for d in "$HOME/.local/bin" "/usr/local/bin" "$HOME/bin"; do
  if [ -d "$d" ] && [ -w "$d" ]; then PREFIX="$d"; break; fi
done
if [ -z "$PREFIX" ]; then
  PREFIX="$HOME/.local/bin"
  mkdir -p "$PREFIX"
fi

step "Starting the daemon"
"$APP/Contents/Resources/bin/ibeji" install --prefix "$PREFIX" \
  || die "Setup failed. Run '$APP/Contents/Resources/bin/ibeji doctor' to see why."

# --- confirm ----------------------------------------------------------------

step "Checking"
CLI="$APP/Contents/Resources/bin/ibeji"
i=0
while [ $i -lt 20 ]; do
  if "$CLI" devices >/dev/null 2>&1; then break; fi
  sleep 0.5
  i=$((i + 1))
done
"$CLI" doctor || true

# A link in a directory the shell never searches is a link nobody can use.
case ":$PATH:" in
  *":$PREFIX:"*) ;;
  *)
    warn "$PREFIX is not on your PATH."
    warn "Add this to your ~/.zprofile, then open a new terminal:"
    printf '        export PATH="%s:$PATH"\n' "$PREFIX"
    ;;
esac

say ""
say "${B}Ìbejì is installed.${X}"
say ""
say "  Open it from Applications, or run ${B}ibeji${X} in a new terminal."
say "  Install it on your other Mac the same way, then pair them:"
say "  both machines must show the same six digits."
say ""
