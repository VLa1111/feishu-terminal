#!/usr/bin/env bash
# 生成发布包：
#   dist/feishu-terminal-<ver>-linux-<arch>.tar.gz   新服务器部署用（含配置样例与安装脚本）
#   dist/feishu-terminal-linux-<arch>                裸二进制，/upgrade 用
#   dist/release.json                                版本清单，/checkupdate 与 /upgrade 读取
#   dist/SHA256SUMS
#
# 用法：bash scripts/release.sh [版本号]
# 在 Mac 上也可以跑，但需要装 Go；没有 Go 时把源码放到任意有 Go 的机器上执行。
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-$(cat VERSION 2>/dev/null || echo 0.0.0)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
NOTES="${NOTES:-}"
# 让 notes 能安全塞进 JSON：去掉换行并按 JSON 规则转义双引号/反斜杠。
NOTES="$(printf '%s' "$NOTES" | tr '\n\r\t' '   ' | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')"
LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}"
OUT=dist

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

if ! command -v go >/dev/null 2>&1; then
  echo "错误：找不到 go。没有 Go 的机器请把本目录同步到有 Go 的服务器后执行 'make dist'。" >&2
  exit 1
fi

rm -rf "$OUT"
mkdir -p "$OUT"

echo "==> feishu-terminal ${VERSION} (commit ${COMMIT})"

for pair in linux/amd64 linux/arm64; do
  os="${pair%/*}"
  arch="${pair#*/}"
  base="feishu-terminal-${VERSION}-${os}-${arch}"
  stage="$OUT/$base"

  echo "==> 构建 ${os}/${arch}"
  mkdir -p "$stage/zsh" "$stage/deploy"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$stage/feishu-terminal" .

  cp README.md .env.sample "$stage/"
  cp zsh/.zshrc "$stage/zsh/.zshrc"
  cp deploy/install.sh "$stage/install.sh"
  cp deploy/feishu-terminal.service "$stage/deploy/feishu-terminal.service"
  chmod 0755 "$stage/feishu-terminal" "$stage/install.sh"

  # 裸二进制：/upgrade 直接下载它，按当前架构挑选
  raw="$OUT/feishu-terminal-${os}-${arch}"
  cp "$stage/feishu-terminal" "$raw"

  # 部署包
  tar -C "$OUT" -czf "$OUT/${base}.tar.gz" "$base"
  rm -rf "$stage"
done

amd64_sha="$(sha256 "$OUT/feishu-terminal-linux-amd64")"
arm64_sha="$(sha256 "$OUT/feishu-terminal-linux-arm64")"

cat > "$OUT/release.json" <<EOF
{
  "version": "${VERSION}",
  "notes": "${NOTES}",
  "built_at": "${BUILD_TIME}",
  "artifacts": {
    "linux-amd64": {"url": "feishu-terminal-linux-amd64", "sha256": "${amd64_sha}"},
    "linux-arm64": {"url": "feishu-terminal-linux-arm64", "sha256": "${arm64_sha}"}
  }
}
EOF

(
  cd "$OUT"
  for f in feishu-terminal-*.tar.gz feishu-terminal-linux-* release.json; do
    [ -f "$f" ] && printf '%s  %s\n' "$(sha256 "$f")" "$f"
  done
) > "$OUT/SHA256SUMS"

echo
echo "==> 完成：$OUT/"
ls -lh "$OUT"
echo
echo "部署到新服务器："
echo "  scp $OUT/feishu-terminal-${VERSION}-linux-amd64.tar.gz user@host:/tmp/"
echo "  ssh user@host 'tar xzf /tmp/feishu-terminal-${VERSION}-linux-amd64.tar.gz && cd feishu-terminal-${VERSION}-linux-amd64 && sudo ./install.sh'"
echo
echo "在线升级：把 $OUT/ 放到任意静态 HTTP 服务下，然后："
echo "  /upgrade https://<host>/release.json"
