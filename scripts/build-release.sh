#!/bin/bash
set -e

VERSION="${1:-dev}"
OUTDIR="dist"

mkdir -p "$OUTDIR"

# Build for all targets
for OS in darwin linux; do
  for ARCH in amd64 arm64; do
    echo "Building $OS/$ARCH..."
    GOOS=$OS GOARCH=$ARCH go build -ldflags="-s -w" -o "$OUTDIR/llm-proxy-$OS-$ARCH" .
  done
done

# Create tarballs for Homebrew
cd "$OUTDIR"
for f in llm-proxy-darwin-*; do
  tar -czf "${f}.tar.gz" "$f"
done

echo "Build complete. Artifacts in $OUTDIR/"
