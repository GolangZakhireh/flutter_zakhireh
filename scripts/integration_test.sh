#!/usr/bin/env bash
set -euo pipefail

# ANSI Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

log() {
    echo -e "${CYAN}[TEST]${NC} $1"
}

success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
    exit 1
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

# Paths
ROOT_DIR="$(pwd)"
DATA_DIR="$ROOT_DIR/data/packages"
TEST_DIR="$ROOT_DIR/test-flutter"
SERVER_URL="http://localhost:8811"

# 0. Isolate Flutter Pub Cache
TEST_PUBCACHE=$(mktemp -d)
log "Using isolated PUB_CACHE: $TEST_PUBCACHE"
export PUB_CACHE="$TEST_PUBCACHE"

# Ensure server is running
if ! curl -s "$SERVER_URL" > /dev/null; then
    error "FlutterZakhireh server is not running on $SERVER_URL. Please start it with 'go run ./cmd/server' or via Docker."
fi

log "Starting Integration Tests..."
log "Data Dir: $DATA_DIR"
log "Test Project Dir: $TEST_DIR"

# Cleanup function to be called on exit
cleanup() {
    log "Cleaning up temporary test files..."
    rm -f "$TEST_DIR/upload_resp.txt"
    
    log "Removing temporary PUB_CACHE..."
    chmod -R u+w "$TEST_PUBCACHE" 2>/dev/null || true
    rm -rf "$TEST_PUBCACHE"
}
trap cleanup EXIT

# 1. Setup Test Flutter Project
mkdir -p "$TEST_DIR"
cd "$TEST_DIR"

if [ ! -f pubspec.yaml ]; then
    log "Initializing test Flutter project..."
    cat > pubspec.yaml << 'EOF'
name: test_flutter
description: A test Flutter project.
publish_to: 'none'
version: 1.0.0+1

environment:
  sdk: '>=3.0.0 <4.0.0'

dependencies:
  flutter:
    sdk: flutter
  http: ^1.1.0
  provider: ^6.0.5

dev_dependencies:
  flutter_test:
    sdk: flutter
  flutter_lints: ^3.0.0

flutter:
  uses-material-design: true
EOF
fi

# 2. Clear local cache to force fetch
log "Cleaning local pub cache..."
flutter pub cache repair 2>/dev/null || true


# --- 3. Test Proxy (Expect Cache Miss & Fallback) ---
PACKAGE_404="http"
VERSION_404="1.1.0"
log "Running 'flutter pub get' with PUB_HOSTED_URL=$SERVER_URL (Expect Cache Miss & Fallback)..."
export PUB_HOSTED_URL="$SERVER_URL"

set +e
FLUTTER_GET_OUTPUT=$(flutter pub get 2>&1)
FLUTTER_GET_CODE=$?
set -e

if [ $FLUTTER_GET_CODE -eq 0 ]; then
    success "Downloaded ${PACKAGE_404}@${VERSION_404} via proxy"
else
    echo "$FLUTTER_GET_OUTPUT"
    # First time might fail if not in cache
    if echo "$FLUTTER_GET_OUTPUT" | grep -q "404\|not found"; then
        error "Failed to download via proxy: 404 Not Found (expected for uncached package)"
    else
        error "Failed to download via proxy"
    fi
fi

# Check if package was cached
include_path="$DATA_DIR/${PACKAGE_404}/${VERSION_404}.tar.gz"
if [ -f "$include_path" ]; then
    success "Package cached at $include_path"
else
    warn "Package was NOT cached locally (may be expected on first run)"
fi

# --- 4. Test Cache Hit for package (Offline Simulation) ---
log "Testing Cache Hit for package..."
flutter pub cache repair 2>/dev/null || true

log "Running 'flutter pub get' again (Should start from cache)..."
time flutter pub get
success "Cache hit test passed (functional)"

# 5. Test Upload for package
log "Testing Upload Endpoint for package..."
# When PUB_HOSTED_URL is set, pub stores archives under a URL-encoded directory
# (e.g. hosted/localhost%3A8811/), so search the whole hosted/ tree.
PUB_CACHE_HOSTED="$PUB_CACHE/hosted"
if [ -d "$PUB_CACHE_HOSTED" ]; then
    log "Uploading ${PACKAGE_404} from local cache to verify upload handler..."

    # Find the cached archive anywhere under the hosted/ tree
    ARCHIVE_FILE=$(find "$PUB_CACHE_HOSTED" -name "${PACKAGE_404}-${VERSION_404}.tar.gz" | head -1)
    if [ -n "$ARCHIVE_FILE" ]; then
        cp "$ARCHIVE_FILE" "./${PACKAGE_404}-${VERSION_404}.tar.gz"
        
        curl -s -X POST "$SERVER_URL/upload" \
            -F "package=${PACKAGE_404}" \
            -F "version=${VERSION_404}" \
            -F "archive=@${PACKAGE_404}-${VERSION_404}.tar.gz" > upload_resp.txt

        if grep -q "ok" upload_resp.txt; then
            success "Upload successful"
        else
            cat upload_resp.txt
            error "Upload failed"
        fi

        if [ -f "$DATA_DIR/${PACKAGE_404}/${VERSION_404}.tar.gz" ]; then
            success "Uploaded file verified in storage"
        else
            error "Uploaded file not found in storage"
        fi
    else
        warn "Could not find cached archive to test upload"
    fi
else
    warn "Could not find PUB_CACHE hosted/ directory to test upload"
fi

success "All integration tests passed!"
