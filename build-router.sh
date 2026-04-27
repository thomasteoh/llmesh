#!/usr/bin/env bash
#
# build-router.sh — Build the llmesh router Docker image
#
# Usage:
#   ./build-router.sh [OPTIONS]
#
# Options:
#   --no-clients        Exclude client binaries/downloads from the image
#   --include-clients   Include client binaries/downloads (default)
#   --version TAG       Set the VERSION build arg (default: dev)
#   --tag TAG           Set the Docker image tag (default: llmesh-router:latest)
#   -h, --help          Show this help message
#
# Examples:
#   ./build-router.sh                          # Build with clients, tag: llmesh-router:latest
#   ./build-router.sh --no-clients             # Build without clients
#   ./build-router.sh --version v0.1.0         # Build with clients, version=v0.1.0
#   ./build-router.sh --no-clients --version v0.1.0 --tag myrepo/llmesh-router:v0.1.0
#
# Default: includes client binaries, man page, docker-compose file, and config example.
# Use --no-clients to build a minimal image that contains only the router binary.

set -euo pipefail

# ── Defaults ────────────────────────────────────────────────────────────────
INCLUDE_CLIENTS="true"
VERSION="dev"
TAG="llmesh-router:latest"
CONTEXT_DIR="."

# ── Argument parsing ────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
    case "$1" in
        --no-clients)
            INCLUDE_CLIENTS="false"
            shift
            ;;
        --include-clients)
            INCLUDE_CLIENTS="true"
            shift
            ;;
        --version)
            VERSION="$2"
            shift 2
            ;;
        --tag)
            TAG="$2"
            shift 2
            ;;
        -h|--help)
            sed -n '2,30p' "$0"
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            echo "Use --help for usage information." >&2
            exit 1
            ;;
    esac
done

# ── Build ────────────────────────────────────────────────────────────────────
echo "Building llmesh router image:"
echo "  VERSION   = ${VERSION}"
echo "  CLIENTS   = ${INCLUDE_CLIENTS}"
echo "  TAG       = ${TAG}"
echo ""

docker build \
    --build-arg "VERSION=${VERSION}" \
    --build-arg "INCLUDE_CLIENTS=${INCLUDE_CLIENTS}" \
    -f router/Dockerfile \
    -t "${TAG}" \
    .

echo ""
echo "✅ Image built: ${TAG}"
