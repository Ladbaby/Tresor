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

set -euo pipefail

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
info "Detecting latest release..."

if command -v curl >/dev/null 2>&1; then
    LATEST=$(curl -fsSL "$GITHUB_API")
else
    LATEST=$(wget -q -O - "$GITHUB_API")
fi

# Extract tag_name (e.g. "v0.1.0")
VERSION=$(printf '%s' "$LATEST" | grep -o '"tag_name":"[^"]*"' | head -1 | cut -d'"' -f4)
[ -z "$VERSION" ] && fail "Could not determine latest release version."

ok "Latest release: $VERSION"

# ── Download ───────────────────────────────────────────────────────────
URL="${GITHUB_DLS}/${VERSION}/${ASSET}-${VERSION}.gz"
TMPFILE=$(mktemp /tmp/tresor-setup-XXXXXX.gz)
trap 'rm -f "$TMPFILE" "${TMPFILE%.gz}"' EXIT

info "Downloading ${ASSET} from ${URL}..."

if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$TMPFILE" "$URL" || fail "Download failed. Check your network or the version."
else
    wget -q -O "$TMPFILE" "$URL" || fail "Download failed. Check your network or the version."
fi

[ -s "$TMPFILE" ] || fail "Downloaded file is empty."

# ── Extract ────────────────────────────────────────────────────────────
info "Extracting binary..."
gzip -d "$TMPFILE"
BINARY="${TMPFILE%.gz}"
[ -s "$BINARY" ] || fail "Extracted binary is empty."

# ── Install to ~/.local/bin ────────────────────────────────────────────
mkdir -p "$BIN_DIR"
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
    CONFIG_URL="https://raw.githubusercontent.com/$REPO/main/config.example.yaml"
    info "Downloading example config..."

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
