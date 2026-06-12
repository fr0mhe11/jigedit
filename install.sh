#!/bin/bash

# 에러 발생 시 즉시 스크립트 중단
set -e

echo "Starting jigedit installation..."

# 1. 시스템 패키지 관리자 감지 및 디펜던시 설치
echo "Checking and installing dependencies (zenity, xclip)..."

if command -v apt-get >/dev/null 2>&1; then
    echo "Ubuntu/Debian detected. Installing with apt..."
    sudo apt-get update -y
    sudo apt-get install -y zenity xclip
elif command -v pacman >/dev/null 2>&1; then
    echo "Arch Linux detected. Installing with pacman..."
    sudo pacman -Sy --noconfirm zenity xclip
elif command -v dnf >/dev/null 2>&1; then
    echo "Fedora/RHEL detected. Installing with dnf..."
    sudo dnf install -y zenity xclip
else
    echo "⚠️⚠️⚠️Unsupported package manager. Please install 'zenity' and 'xclip' manually.⚠️⚠️⚠️"
fi

# 2. 최신 릴리즈 바이너리 다운로드
echo "Downloading the latest release of jigedit..."
curl -sL "https://github.com/fr0mhe11/jigedit/releases/latest/download/jigedit" -o /tmp/jigedit

# 3. 권한 부여 및 시스템 폴더로 이동
echo "Moving binary to /usr/local/bin (sudo required)..."
chmod +x /tmp/jigedit
sudo mv /tmp/jigedit /usr/local/bin/jigedit

echo "jigedit installed successfully! Try running 'jigedit -h'"
