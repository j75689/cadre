#!/bin/sh
# install.sh installs the cadre CLI from a GitHub release:
#
#   curl -fsSL https://raw.githubusercontent.com/j75689/cadre/main/hack/install.sh | sh
#
# It downloads the archive for this OS and architecture (darwin or linux,
# amd64 or arm64) and the release's checksums, refuses an archive whose
# checksum does not match or is not listed, and installs cadre. It never
# runs sudo; where it may not write, it says how to run it with sudo.
#
#   CADRE_VERSION      the release, as v1.2.3 or 1.2.3; by default the
#                      latest, which is never a pre-release
#   CADRE_INSTALL_DIR  where to install cadre; by default /usr/local/bin
#                      when it is writable, else ~/.local/bin
#   CADRE_BASE_URL     where the releases' files are, by tag; for tests
#   CADRE_LATEST_URL   the page that redirects to the latest release's;
#                      for tests
#
# The latest release is found from GitHub's redirect, not its API, which
# allows few unauthenticated requests per address.
set -eu

OWNER=j75689
REPO=cadre
BASE_URL=${CADRE_BASE_URL:-https://github.com/$OWNER/$REPO/releases/download}
LATEST_URL=${CADRE_LATEST_URL:-https://github.com/$OWNER/$REPO/releases/latest}
SCRIPT_URL=https://raw.githubusercontent.com/$OWNER/$REPO/main/hack/install.sh

say() { printf 'cadre install: %s\n' "$*" >&2; }
fail() {
  say "$*"
  exit 1
}

if command -v curl >/dev/null 2>&1; then
  http=curl
elif command -v wget >/dev/null 2>&1; then
  http=wget
else
  fail "neither curl nor wget is installed"
fi
command -v tar >/dev/null 2>&1 || fail "tar is not installed"
if command -v sha256sum >/dev/null 2>&1; then
  check="sha256sum -c"
elif command -v shasum >/dev/null 2>&1; then
  check="shasum -a 256 -c"
else
  fail "neither sha256sum nor shasum is installed, so a download cannot be verified"
fi

# fetch URL FILE downloads URL into FILE.
fetch() {
  if [ "$http" = curl ]; then
    curl -fsSL -o "$2" "$1"
  else
    wget -q -O "$2" "$1"
  fi
}

# redirected URL prints where URL redirects to.
redirected() {
  if [ "$http" = curl ]; then
    curl -fsSIL -o /dev/null -w '%{url_effective}' "$1"
  else
    # wget follows the redirects and prints each hop's response headers
    # (-S), indented, with "Location" in any case a proxy left it in; GNU
    # wget also logs "Location: <url> [following]", the same URL. wget2
    # may leave Location out of that dump (2.2 always does, 2.3 for a
    # lowercase header, as over HTTP/2), and logs each redirect it
    # follows: "Adding URL: <url>" before 2.3, "Enqueuing <url>" from
    # 2.3. The last URL is the release's page.
    wget -S -O /dev/null "$1" 2>&1 |
      sed -n -e 's/^ *[Ll]ocation: *//p' -e 's/^Adding URL: *//p' -e 's/^Enqueuing *//p' |
      sed 's/ \[following\]$//' | tr -d '\r' | tail -n 1
  fi
}

case $(uname -s) in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) os= ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) arch= ;;
esac
if [ -z "$os" ] || [ -z "$arch" ]; then
  fail "there is no archive for $(uname -s) $(uname -m) to install from here; download one by hand from https://github.com/$OWNER/$REPO/releases (for Windows, the .zip)"
fi

tmp=$(mktemp -d)
# staged is the binary copied into place but not yet renamed over cadre.
staged=
trap 'rm -rf "$tmp"; if [ -n "$staged" ]; then rm -f "$staged"; fi' EXIT
trap 'exit 1' INT TERM

version=${CADRE_VERSION:-}
if [ -z "$version" ]; then
  latest=$(redirected "$LATEST_URL") || latest=
  case $latest in
    */tag/*) version=${latest##*/tag/} ;;
    *) fail "could not find the latest release from $LATEST_URL (there may be none yet); set CADRE_VERSION" ;;
  esac
fi
version=${version#v}
tag=v$version
archive=cadre_${version}_${os}_${arch}.tar.gz
sums=cadre_${version}_checksums.txt

say "downloading cadre $tag for $os/$arch"
fetch "$BASE_URL/$tag/$archive" "$tmp/$archive" || fail "could not download $BASE_URL/$tag/$archive"
fetch "$BASE_URL/$tag/$sums" "$tmp/$sums" || fail "could not download $BASE_URL/$tag/$sums"

# Only the archive's own line is checked: a missing one is a refusal.
awk -v f="$archive" '$2 == f' "$tmp/$sums" >"$tmp/check.txt"
[ -s "$tmp/check.txt" ] || fail "$sums lists no checksum for $archive: not installing"
# $check is two or three words, split on purpose.
# shellcheck disable=SC2086
(cd "$tmp" && $check check.txt >/dev/null 2>&1) || fail "the checksum of $archive does not match $sums: not installing"

dir=${CADRE_INSTALL_DIR:-}
if [ -z "$dir" ]; then
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    dir=/usr/local/bin
  elif [ -n "${HOME:-}" ]; then
    dir=$HOME/.local/bin
  else
    fail "/usr/local/bin is not writable and HOME is not set: set CADRE_INSTALL_DIR"
  fi
fi
mkdir -p "$dir" 2>/dev/null || true
if [ ! -d "$dir" ] || [ ! -w "$dir" ]; then
  fail "cannot write to $dir; run it with sudo instead: curl -fsSL $SCRIPT_URL | sudo CADRE_VERSION=$tag CADRE_INSTALL_DIR=$dir sh"
fi

tar -xzf "$tmp/$archive" -C "$tmp" cadre
# Copied beside, then renamed over: a cadre that is running is not hit,
# and the trap removes the copy if this stops halfway.
staged=$dir/.cadre.$$
cp "$tmp/cadre" "$staged"
chmod 0755 "$staged"
mv -f "$staged" "$dir/cadre"
staged=

if out=$("$dir/cadre" version 2>&1); then
  say "installed $dir/cadre"
  printf '%s\n' "$out"
else
  fail "installed $dir/cadre, but it did not run: $out"
fi
case ":${PATH:-}:" in
  *":$dir:"*) ;;
  *) say "$dir is not on your PATH; add it, for example: export PATH=\"$dir:\$PATH\"" ;;
esac
