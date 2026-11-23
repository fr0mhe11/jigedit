#!/bin/bash

# 에러 발생 시 즉시 스크립트 중단
set -e

echo "Starting jigedit installation..."
echo ""

# 1. 최신 릴리즈 바이너리 다운로드
echo "Downloading the latest release of jigedit..."
curl -sL "https://github.com/fr0mhe11/jigedit/releases/latest/download/jigedit" -o /tmp/jigedit

# 2. 권한 부여 및 시스템 폴더로 이동
echo "Moving binary to /usr/local/bin (sudo required)..."
chmod +x /tmp/jigedit
sudo mv /tmp/jigedit /usr/local/bin/jigedit

echo ""
echo "jigedit installed successfully! Try running 'jigedit -h'"
