#!/bin/bash
echo "Installing jigedit..."

# 최신 릴리즈 다운로드 (Linux amd64 기준)
curl -sL "https://github.com/fr0mhe11/jigedit/releases/latest/download/jigedit" -o /tmp/jigedit

# 실행 권한 부여 및 시스템 폴더로 이동 (sudo 필요)
chmod +x /tmp/jigedit
sudo mv /tmp/jigedit /usr/local/bin/

echo "✅ jigedit installed successfully! Try running 'jigedit -h'"
