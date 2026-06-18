#!/usr/bin/env bash
# Tresor — one-line setup script
# Installs the latest release binary to ~/.local/bin
# Creates a config skeleton in ~/.config/tresor/config.yaml
# Supports: Linux (amd64, arm64), macOS (amd64, arm64)
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Ladbaby/Tresor/main/setup.sh | bash
#   # or
#   ./setup.sh
#   # include pre-releases:
#   ./setup.sh --prerelease

set -euo pipefail

# ── Flags ──────────────────────────────────────────────────────────────
INCLUDE_PRERELEASE=false
while [[ "$#" -gt 0 ]]; do
    case "$1" in
        --prerelease) INCLUDE_PRERELEASE=true; shift ;;
        *) shift ;;
    esac
done
# Allow env var override: TRESOR_PRERELEASE=true
[ "${TRESOR_PRERELEASE:-false}" = "true" ] && INCLUDE_PRERELEASE=true

# ── Constants ──────────────────────────────────────────────────────────
REPO="Ladbaby/Tresor"
BIN_DIR="$HOME/.local/bin"
CONFIG_DIR="$HOME/.config/tresor"
CONFIG_FILE="$CONFIG_DIR/config.yaml"
GITHUB_API="https://api.github.com/repos/$REPO/releases/latest"
GITHUB_DLS="https://github.com/$REPO/releases/download"

# ── Colors (if terminal supports them) ─────────────────────────────────
if [[ -t 1 ]]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[0;33m'
    CYAN='\033[0;36m'
    NC='\033[0m'
else
    RED='' GREEN='' YELLOW='' CYAN='' NC=''
fi

info()  { echo -e "${CYAN}ℹ  $1${NC}"; }
ok()    { echo -e "${GREEN}✓  $1${NC}"; }
warn()  { echo -e "${YELLOW}⚠  $1${NC}"; }
fail()  { echo -e "${RED}✗  $1${NC}"; exit 1; }

# ── Check prerequisites ────────────────────────────────────────────────
command -v curl >/dev/null 2>&1 || { command -v wget >/dev/null 2>&1 || fail "curl or wget is required but not installed."; }
command -v gzip >/dev/null 2>&1  || fail "gzip is required but not installed."

# ── Detect OS / arch ───────────────────────────────────────────────────
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$OS" in
    linux)  OS_PKG="linux" ;;
    darwin) OS_PKG="darwin" ;;
    *)      fail "Unsupported platform: $OS ($(uname -a)). This script only supports Linux and macOS.\nFor other platforms, please build from source: https://ladbaby.github.io/Tresor-docs/docs/user/getting-started/installation" ;;
esac

case "$ARCH" in
    x86_64|amd64) ARCH_PKG="amd64" ;;
    aarch64|arm64) ARCH_PKG="arm64" ;;
    *)             fail "Unsupported architecture: $ARCH (on $OS)." ;;
esac

ASSET="tresor-${OS_PKG}-${ARCH_PKG}"

# ── Fetch latest release ───────────────────────────────────────────────
# Allow version pinning via TRESOR_VERSION env var (e.g. TRESOR_VERSION=v0.1.0)
if [ -n "${TRESOR_VERSION:-}" ]; then
    ok "Using pinned version: $TRESOR_VERSION"
    VERSION="$TRESOR_VERSION"
else
    # Use /releases endpoint (includes prereleases) when --prerelease flag is set
    if [ "$INCLUDE_PRERELEASE" = "true" ]; then
        RELEASE_API="https://api.github.com/repos/$REPO/releases"
        info "Detecting latest release (including pre-releases)..."
    else
        RELEASE_API="$GITHUB_API"
        info "Detecting latest release..."
    fi

    if command -v curl >/dev/null 2>&1; then
        LATEST=$(curl -fsSL "$RELEASE_API") || fail "Cannot fetch release info from $RELEASE_API. Check your network connection, DNS, and TLS certificates (install ca-certificates if needed)."
    else
        LATEST=$(wget -q -O - "$RELEASE_API") || fail "Cannot fetch release info from $RELEASE_API. Check your network connection."
    fi

    # /releases returns a JSON array — pick the first entry
    # /releases/latest returns a single JSON object
    # In both cases, extract tag_name from the first occurrence
    VERSION=$(printf '%s' "$LATEST" | grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | grep -o '"[^"]*"$' | tr -d '"')
    [ -z "$VERSION" ] && fail "Could not determine latest release version."

    if [ "$INCLUDE_PRERELEASE" = "true" ]; then
        ok "Latest release: $VERSION (pre-release)"
    else
        ok "Latest release: $VERSION"
    fi
fi

# ── Download ───────────────────────────────────────────────────────────
URL="${GITHUB_DLS}/${VERSION}/${ASSET}-${VERSION}.gz"
TMPFILE=$(mktemp /tmp/tresor-setup-XXXXXX.gz)
CHECKSUM_FILE=$(mktemp /tmp/tresor-checksum-XXXXXX)
trap 'rm -f "$TMPFILE" "${TMPFILE%.gz}" "$CHECKSUM_FILE"' EXIT

info "Downloading ${ASSET} from ${URL}..."

if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$TMPFILE" "$URL" || fail "Download failed. Check your network or the version."
else
    wget -q -O "$TMPFILE" "$URL" || fail "Download failed. Check your network or the version."
fi

[ -s "$TMPFILE" ] || fail "Downloaded file is empty."

# ── Verify checksum ────────────────────────────────────────────────────
CHECKSUM_URL="${GITHUB_DLS}/${VERSION}/checksums.txt"
info "Downloading checksums..."

if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$CHECKSUM_FILE" "$CHECKSUM_URL" || warn "Could not download checksums. Skipping verification."
else
    wget -q -O "$CHECKSUM_FILE" "$CHECKSUM_URL" || warn "Could not download checksums. Skipping verification."
fi

if [ -s "$CHECKSUM_FILE" ]; then
    ASSET_BASENAME="${ASSET}-${VERSION}.gz"
    EXPECTED=$(grep "$ASSET_BASENAME" "$CHECKSUM_FILE" || true)
    if [ -n "$EXPECTED" ]; then
        if command -v sha256sum >/dev/null 2>&1; then
            ACTUAL=$(sha256sum "$TMPFILE" | awk '{print $1}')
            EXPECTED_HASH=$(echo "$EXPECTED" | awk '{print $1}')
            if [ "$ACTUAL" != "$EXPECTED_HASH" ]; then
                fail "Checksum mismatch for $ASSET_BASENAME!\n  Expected: $EXPECTED_HASH\n  Actual:   $ACTUAL\n  The download may be corrupted or tampered with."
            fi
            ok "Checksum verified."
        else
            warn "sha256sum not available, skipping checksum verification."
        fi
    else
        warn "Asset $ASSET_BASENAME not found in checksums.txt, skipping verification."
    fi
fi

# ── Extract ────────────────────────────────────────────────────────────
info "Extracting binary..."
gzip -d "$TMPFILE"
BINARY="${TMPFILE%.gz}"
[ -s "$BINARY" ] || fail "Extracted binary is empty."

# ── Install to ~/.local/bin ────────────────────────────────────────────
mkdir -p "$BIN_DIR"

if [ -f "$BIN_DIR/tresor" ]; then
    EXISTING_VER=$("$BIN_DIR/tresor" version 2>/dev/null || echo "unknown")
    warn "Tresor already installed: $EXISTING_VER"
    if [ "$VERSION" = "$(echo "$EXISTING_VER" | grep -o 'v[^ ]*' || true)" ]; then
        info "Already at version $VERSION. Overwriting..."
    fi
    cp "$BIN_DIR/tresor" "$BIN_DIR/tresor.bak"
    info "Backup saved: $BIN_DIR/tresor.bak"
fi

cp "$BINARY" "$BIN_DIR/tresor"
chmod +x "$BIN_DIR/tresor"

ok "Installed: $BIN_DIR/tresor ($VERSION)"

# Check if ~/.local/bin is in PATH
if [[ ":$PATH:" != *":$BIN_DIR:"* ]]; then
    warn "$BIN_DIR is not in your PATH."
    echo "   Add this to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
    echo "   export PATH=\"$BIN_DIR:\$PATH\""
    echo ""
fi

# ── Create config if missing ───────────────────────────────────────────
if [ -f "$CONFIG_FILE" ]; then
    ok "Config exists: $CONFIG_FILE (skipped)"
else
    mkdir -p "$CONFIG_DIR"
    CONFIG_URL="https://raw.githubusercontent.com/$REPO/${VERSION}/config.example.yaml"
    info "Downloading example config (from $VERSION)..."

    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$CONFIG_URL" -o "$CONFIG_FILE" || warn "Could not download example config. Create $CONFIG_FILE manually."
    else
        wget -q -O "$CONFIG_FILE" "$CONFIG_URL" || warn "Could not download example config. Create $CONFIG_FILE manually."
    fi

    [ -s "$CONFIG_FILE" ] && ok "Config created: $CONFIG_FILE"
fi

# ── Summary ────────────────────────────────────────────────────────────
echo ""
echo "========================================"
echo "  Tresor setup complete!"
echo "========================================"
echo ""
echo "  Binary:  $BIN_DIR/tresor ($VERSION)"
echo "  Config:  $CONFIG_FILE"
echo ""
echo "  Next steps:"
echo "  1. Edit $CONFIG_FILE"
echo "     Add your provider API keys"
echo ""
echo "  2. Start the daemon:"
echo "     tresor run --config $CONFIG_FILE"
echo ""
echo "  3. Point your apps to: http://127.0.0.1:11510"
echo "  4. Open http://127.0.0.1:11510 in your browser for the web UI"
echo ""
