#!/bin/bash

# 에러 발생 시 즉시 스크립트 중단
set -e

echo "Starting jigedit installation..."
echo ""

# 디스플레이 서버 환경 자동 감지
if [ -n "$WAYLAND_DISPLAY" ]; then
    DETECTED_ENV="Wayland"
    DEFAULT_CHOICE="1"
elif [ -n "$DISPLAY" ]; then
    DETECTED_ENV="X11"
    DEFAULT_CHOICE="2"
else
    DETECTED_ENV="Unknown"
    DEFAULT_CHOICE="3"
fi

echo "[$DETECTED_ENV 환경이 감지되었습니다]"
echo "클립보드 연동을 위한 패키지를 선택해 주세요:"
echo "1) wl-clipboard (Wayland 환경 추천)"
echo "2) xclip (X11 환경 추천)"
echo "3) 둘 다 설치 (호환성 목적)"
echo "4) 설치 안 함 (이미 설치되어 있거나 수동 설치)"

# 💡 수정됨: curl | bash 로 실행해도 키보드 입력을 정상적으로 대기하도록 </dev/tty 추가
read -p "번호를 선택하세요 [1-4] (엔터를 누르면 추천값 $DEFAULT_CHOICE 선택): " USER_CHOICE </dev/tty

# 사용자가 그냥 엔터를 쳤을 경우 기본값 할당
if [ -z "$USER_CHOICE" ]; then
    USER_CHOICE=$DEFAULT_CHOICE
fi

CLIP_PKG=""
case $USER_CHOICE in
    1) CLIP_PKG="wl-clipboard" ;;
    2) CLIP_PKG="xclip" ;;
    3) CLIP_PKG="wl-clipboard xclip" ;;
    4) CLIP_PKG="" ;;
    *) 
       echo "잘못된 입력입니다. 기본값($DEFAULT_CHOICE)으로 진행합니다."
       if [ "$DEFAULT_CHOICE" == "1" ]; then CLIP_PKG="wl-clipboard"; else CLIP_PKG="xclip"; fi 
       ;;
esac

echo ""
# 1. 시스템 패키지 관리자 감지 및 디펜던시 설치
echo "Checking and installing dependencies (zenity, $CLIP_PKG)..."

if command -v apt-get >/dev/null 2>&1; then
    echo "Ubuntu/Debian detected. Installing with apt..."
    sudo apt-get update -y
    sudo apt-get install -y zenity $CLIP_PKG
elif command -v pacman >/dev/null 2>&1; then
    echo "Arch Linux detected. Installing with pacman..."
    sudo pacman -Sy --noconfirm zenity $CLIP_PKG
elif command -v dnf >/dev/null 2>&1; then
    echo "Fedora/RHEL detected. Installing with dnf..."
    sudo dnf install -y zenity $CLIP_PKG
else
    echo "⚠️⚠️⚠️Unsupported package manager. Please install 'zenity' and your clipboard utility manually.⚠️⚠️⚠️"
fi

echo ""
# 2. 최신 릴리즈 바이너리 다운로드
echo "Downloading the latest release of jigedit..."
curl -sL "https://github.com/fr0mhe11/jigedit/releases/latest/download/jigedit" -o /tmp/jigedit

# 3. 권한 부여 및 시스템 폴더로 이동
echo "Moving binary to /usr/local/bin (sudo required)..."
chmod +x /tmp/jigedit
sudo mv /tmp/jigedit /usr/local/bin/jigedit

echo ""
echo "jigedit installed successfully! Try running 'jigedit -h'"
