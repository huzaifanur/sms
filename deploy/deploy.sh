#!/usr/bin/env bash
# Builds smsd and activates it as a new release on your server.
# Usage: SMSD_SERVER=user@your-server SMSD_DOMAIN=sms.example.com deploy/deploy.sh
# Optional: SMSD_SSH_KEY (default ~/.ssh/id_ed25519), SMSD_GOARCH (default arm64).
set -euo pipefail
cd "$(dirname "$0")/.."
server=${SMSD_SERVER:?Set SMSD_SERVER, e.g. ubuntu@your-server}
domain=${SMSD_DOMAIN:?Set SMSD_DOMAIN, e.g. sms.example.com}
ssh_args=(-i "${SMSD_SSH_KEY:-$HOME/.ssh/id_ed25519}" -o BatchMode=yes -o StrictHostKeyChecking=yes)
release=$(date -u +%Y%m%dT%H%M%SZ)
bundle=$(mktemp /tmp/smsd-release.XXXXXX.tar.gz)
trap 'rm -f "$bundle"' EXIT
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=${SMSD_GOARCH:-arm64} go build -trimpath -ldflags='-s -w' -o bin/smsd ./cmd/smsd
tar -czf "$bundle" -C bin smsd -C ../deploy smsd.service nginx-site.conf
ssh "${ssh_args[@]}" "$server" "cat > /tmp/smsd-$release.tar.gz" < "$bundle"
ssh "${ssh_args[@]}" "$server" "sudo bash -s -- '$release' '$domain'" < deploy/activate.sh
