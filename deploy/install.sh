#!/usr/bin/env bash
# 新服务器安装脚本：在解压后的目录里运行
#
#   sudo ./install.sh
#
# 可选环境变量：
#   PREFIX=/opt/feishu-terminal   安装目录
#   SERVICE=feishu-terminal       systemd 服务名
#   RUN_USER=<user>              运行用户（默认 sudo 发起者；bot 的 shell 权限就是它的权限）
#
# 升级时重复执行本脚本即可：二进制会替换，.env 不会被覆盖。
set -euo pipefail

PREFIX="${PREFIX:-/opt/feishu-terminal}"
SERVICE="${SERVICE:-feishu-terminal}"
RUN_USER="${RUN_USER:-${SUDO_USER:-root}}"

if [ "$(id -u)" -ne 0 ]; then
  echo "请用 sudo 运行：sudo ./install.sh" >&2
  exit 1
fi
if [ ! -x ./feishu-terminal ]; then
  echo "找不到 ./feishu-terminal —— 请在解压后的发布目录里运行本脚本。" >&2
  exit 1
fi
if ! id -u "$RUN_USER" >/dev/null 2>&1; then
  echo "运行用户 $RUN_USER 不存在。可用 RUN_USER=<用户> sudo ./install.sh" >&2
  exit 1
fi

# 依赖检查：缺了不中断安装，但运行时会失败。
for bin in tmux zsh; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "警告：找不到 $bin，bot 启动后会失败。请先安装（apt install $bin / yum install $bin）。" >&2
  fi
done

install -d -m 0755 "$PREFIX" "$PREFIX/zsh"
install -m 0755 feishu-terminal "$PREFIX/feishu-terminal"
install -m 0644 zsh/.zshrc zsh/.zshenv "$PREFIX/zsh/"

# 首次安装生成 .env；升级时保留已有配置。
if [ -f .env.sample ] && [ ! -f "$PREFIX/.env" ]; then
  install -m 0600 .env.sample "$PREFIX/.env"
  NEED_CONFIG=1
else
  NEED_CONFIG=0
fi

if [ ! -f deploy/feishu-terminal.service ]; then
  echo "发布包缺少 deploy/feishu-terminal.service" >&2
  exit 1
fi
sed -e "s|@PREFIX@|$PREFIX|g" -e "s|@RUN_USER@|$RUN_USER|g" \
  deploy/feishu-terminal.service > "/etc/systemd/system/${SERVICE}.service"
chmod 0644 "/etc/systemd/system/${SERVICE}.service"

systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null
systemctl restart "$SERVICE"
sleep 1

echo
if [ "$NEED_CONFIG" = "1" ]; then
  echo "首次安装完成，但还没有配置："
  echo "  1) 编辑 $PREFIX/.env，填入 FEISHU_APP_ID / FEISHU_APP_SECRET / FEISHU_ALLOW_OPEN_IDS"
  echo "  2) systemctl restart $SERVICE"
else
  echo "已安装并重启 $SERVICE（配置沿用 $PREFIX/.env）。"
fi
echo
echo "查看状态：systemctl status $SERVICE"
echo "查看日志：journalctl -u $SERVICE -f"
echo "运行用户：$RUN_USER（bot 的 shell 就是这个用户的权限）"
echo
systemctl --no-pager --lines=10 status "$SERVICE" || true
