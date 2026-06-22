#!/usr/bin/env bash
# prime_cache.sh - Downloads popular Flutter packages to seed the FlutterZakhireh cache.

set -euo pipefail

# ANSI Colors
GREEN='\033[0;32m'
BLUE='\033[0;34m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
GRAY='\033[1;30m'
NC='\033[0m'

# Configuration
SERVER_URL="${FLUTTERZAKHIREH_URL:-http://localhost:8811}"
export PUB_HOSTED_URL="$SERVER_URL"
export PUB_CACHE=$(mktemp -d)
trap "rm -rf $PUB_CACHE" EXIT

# Create a temporary Flutter project
TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR $PUB_CACHE" EXIT
cd "$TEMP_DIR"

# Generate pubspec.yaml with all packages at once
cat > pubspec.yaml << 'PUBSPEC'
name: prime_cache
description: A temporary project to prime FlutterZakhireh cache.
publish_to: 'none'
version: 1.0.0+1

environment:
  sdk: '>=3.0.0 <4.0.0'

dependencies:
  flutter:
    sdk: flutter

  # Networking & HTTP
  http: ^1.1.0
  dio: ^5.3.0
  chopper: ^7.0.0

  # State Management
  provider: ^6.0.5
  flutter_bloc: ^8.1.3
  riverpod: ^2.4.0
  get_it: ^7.6.0
  injectable: ^2.3.0

  # Local Storage & Database
  shared_preferences: ^2.2.0
  sqflite: ^2.3.0
  hive: ^2.2.3
  hive_flutter: ^1.1.0
  drift: ^2.12.0
  floor: ^1.4.0

  # Serialization
  json_annotation: ^4.8.1
  freezed_annotation: ^2.4.1
  built_value: ^8.6.0

  # UI & Animation
  flutter_animate: ^4.2.0
  lottie: ^2.6.0
  cached_network_image: ^3.3.0
  shimmer: ^3.0.0
  flutter_staggered_animations: ^1.1.1

  # Navigation
  go_router: ^10.0.0
  auto_route: ^7.8.0

  # Utilities
  equatable: ^2.0.5
  dartz: ^0.10.1
  collection: ^1.17.0
  meta: ^1.9.1
  path: ^1.8.3
  uuid: ^4.2.0

  # Security & Crypto
  encrypt: ^5.0.0
  flutter_secure_storage: ^9.0.0

  # Device & Platform
  device_info_plus: ^9.0.0
  package_info_plus: ^4.2.0
  url_launcher: ^6.1.12
  share_plus: ^7.2.0
  permission_handler: ^11.0.0

  # Image & Media
  image_picker: ^1.0.4
  video_player: ^2.7.0
  audioplayers: ^5.0.0

  # Maps & Location
  google_maps_flutter: ^2.5.0
  geolocator: ^10.1.0

  # Charts & Visualization
  fl_chart: ^0.65.0
  syncfusion_flutter_charts: ^23.1.0

  # Firebase
  firebase_core: ^2.24.0
  firebase_auth: ^4.15.0
  cloud_firestore: ^4.13.0
  firebase_storage: ^11.5.0
  firebase_messaging: ^14.7.0

  # Other Popular Packages
  flutter_svg: ^2.0.9
  flutter_launcher_icons: ^0.13.1
  flutter_native_splash: ^2.3.0
  rename: ^3.0.0

dev_dependencies:
  flutter_test:
    sdk: flutter
  integration_test:
    sdk: flutter
  mocktail: ^1.0.0
  bloc_test: ^9.1.0
  flutter_lints: ^3.0.0

flutter:
  uses-material-design: true
PUBSPEC

echo -e "${BLUE}FlutterZakhireh Cache Priming${NC}"
echo
echo -e "  Proxy: ${GRAY}${SERVER_URL}${NC}"
echo -e "  Temp project: ${GRAY}${TEMP_DIR}${NC}"
echo -e "  Packages: ${GRAY}50+ popular Flutter packages${NC}"
echo
echo -e "  Note: Using flutter_form_builder:^9.0.0 and intl:^0.18.0 for version compatibility.${NC}"
echo

# Run flutter pub get once — downloads everything in one shot
if flutter pub get 2>&1; then
    echo
    echo -e " ${GREEN}Cache priming complete!${NC}"
    echo
    echo "Open the dashboard at ${SERVER_URL} to see cached packages."
else
    echo
    echo -e " ${RED}Cache priming failed.${NC}"
    echo "Check the server logs for upstream errors."
    exit 1
fi
