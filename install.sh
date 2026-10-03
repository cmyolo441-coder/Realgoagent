#!/bin/sh
# nova installer
#
#   curl -fsSL https://raw.githubusercontent.com/cmyolo441-coder/Realgoagent/main/install.sh | sh
#
# Environment:
#   NOVA_VERSION   release tag to install (default: the latest release)
#   NOVA_INSTALL   install directory     (default: $HOME/.local/bin)
#   NOVA_REPO      owner/name            (default: cmyolo441-coder/Realgoagent)
#   NOVA_BASE_URL  download host         (default: https://github.com/$NOVA_REPO)

set -eu

REPO=${NOVA_REPO:-cmyolo441-coder/Realgoagent}
BASE_URL=${NOVA_BASE_URL:-https://github.com/$REPO}
BIN="nova"

info() { printf '%s: %s\n' "$BIN" "$1"; }
warn() { printf '%s: %s\n' "$BIN" "$1" >&2; }
die() {
	warn "$1"
	exit 1
}

need_cmd() { command -v "$1" >/dev/null 2>&1; }

have_fetcher() { need_cmd curl || need_cmd wget; }

fetch() {
	if need_cmd curl; then
		curl -fsSL "$1"
	elif need_cmd wget; then
		wget -qO- "$1"
	else
		die "neither curl nor wget found; install one of them first"
	fi
}

fetch_to() {
	if need_cmd curl; then
		curl -fsSL "$1" -o "$2"
	elif need_cmd wget; then
		wget -qO "$1" -o "$2"
	else
		die "neither curl nor wget found; install one of them first"
	fi
}

need_cmd uname || die "uname not found; unsupported system"
have_fetcher || die "neither curl nor wget found; install one of them first"

# ---- platform -------------------------------------------------------------

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
linux | darwin) ;;
*) die "$os is not supported (Linux and macOS only)" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "architecture $(uname -m) is not supported; build from source instead" ;;
esac

# ---- version --------------------------------------------------------------

if [ -n "${NOVA_VERSION:-}" ]; then
	tag=$NOVA_VERSION
else
	# The releases API is unauthenticated and works with both fetchers.
	latest=$(fetch "https://api.github.com/repos/$REPO/releases/latest" |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
	[ -n "$latest" ] || die "could not look up the latest release"
	tag=$latest
fi

asset="nova_${tag}_${os}_${arch}.tar.gz"
base=$BASE_URL/releases/download/$tag

info "installing $BIN $tag ($os/$arch)"

# ---- download -------------------------------------------------------------

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t nova)
trap 'rm -rf "$tmp"' EXIT INT TERM

info "downloading $asset"
fetch_to "$base/$asset" "$tmp/$asset" || die "download failed: $base/$asset"

# Verify the checksum when the release ships one for this asset.
if fetch_to "$base/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
	if need_cmd sha256sum; then
		sum_cmd="sha256sum"
	elif need_cmd shasum; then
		sum_cmd="shasum -a 256"
	else
		sum_cmd=""
	fi
	if [ -n "$sum_cmd" ]; then
		# Tolerate a leading ./ in the checksum file, as sha256sum sometimes writes.
		want=$(awk -v a="$asset" '$2 == a || $2 == "./" a { print $1; exit }' "$tmp/checksums.txt")
		if [ -n "$want" ]; then
			got=$($sum_cmd "$tmp/$asset" | awk '{print $1}')
			[ "$want" = "$got" ] || die "checksum mismatch for $asset"
			info "checksum ok"
		fi
	fi
else
	warn "no checksums.txt for $tag, skipping checksum verification"
fi

tar -xzf "$tmp/$asset" -C "$tmp" 2>/dev/null ||
	die "could not unpack $asset"

# The archive holds a plain "nova", but tolerate a versioned name too.
bin_src=$tmp/$BIN
if [ ! -f "$bin_src" ]; then
	bin_src=$(find "$tmp" -maxdepth 1 -type f -name "$BIN*" ! -name '*.gz' ! -name '*.md' | head -n 1)
fi
[ -n "$bin_src" ] && [ -f "$bin_src" ] || die "$asset does not contain a $BIN binary"

# ---- install --------------------------------------------------------------

dest=${NOVA_INSTALL:-$HOME/.local/bin}

if mkdir -p "$dest" 2>/dev/null && [ -w "$dest" ]; then
	install_path=$dest/$BIN
	cp "$bin_src" "$install_path"
	chmod +x "$install_path"
elif need_cmd sudo; then
	dest=/usr/local/bin
	info "$dest is not writable, using sudo"
	sudo mkdir -p "$dest"
	sudo cp "$bin_src" "$dest/$BIN"
	sudo chmod +x "$dest/$BIN"
	install_path=$dest/$BIN
else
	die "$dest is not writable and sudo is unavailable; set NOVA_INSTALL to a writable directory"
fi

info "installed $install_path"

case ":$PATH:" in
*":$dest:"*) ;;
*) warn "$dest is not on your PATH; add it with: export PATH=\"$dest:\$PATH\"" ;;
esac

cat <<EOF

try it:

  $BIN --help
  $BIN -models

config lives in ~/.nova/config/config.json, edit it to add providers or API keys.
EOF
