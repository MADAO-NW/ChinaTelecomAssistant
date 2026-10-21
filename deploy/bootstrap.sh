#!/usr/bin/env bash
# 一行式安装引导：从 Release 取回 deploy/install.sh 并执行 install。
#
# 适用于全新机器（不需要先克隆仓库）。本脚本只做三件事：
#   1. 用令牌向 GitHub API 解析最新（或指定）正式版本，找到它的 install.sh 资产；
#   2. 带令牌下载该资产并做轻量校验；
#   3. 导出 GH_TOKEN 后调用 `install.sh install [版本]`，其余全部交给 install.sh。
#
# 私有仓库必须提供令牌（Contents: read）：
#   GH_TOKEN / CTM_GITHUB_TOKEN 环境变量，或 --token-file 指定的文件
#   （默认 /etc/ctm/github-token，权限 0600/0400）。
#
# 用法：
#   sudo bash bootstrap.sh                          # 安装最新正式版，令牌取 /etc/ctm/github-token
#   sudo bash bootstrap.sh v0.1.0                   # 指定版本
#   sudo bash bootstrap.sh --version v0.1.0 --check # 只解析并打印将要执行的命令
#
# 环境变量：CTM_GITHUB_REPO（默认 MADAO-NW/ChinaTelecomAssistant）、
#           CTM_GITHUB_API_BASE（默认 https://api.github.com）。
set -Eeuo pipefail

readonly DEFAULT_REPOSITORY="MADAO-NW/ChinaTelecomAssistant"
readonly DEFAULT_API_BASE="https://api.github.com"
readonly DEFAULT_TOKEN_FILE="/etc/ctm/github-token"
readonly TAG_PATTERN='^v[0-9]+\.[0-9]+\.[0-9]+$'

TEMP_DIR=""
cleanup() {
  if [[ -n ${TEMP_DIR} && -d ${TEMP_DIR} ]]; then
    rm -rf -- "${TEMP_DIR}"
  fi
}
trap cleanup EXIT

die() {
  echo "错误：$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
用法：sudo bash bootstrap.sh [版本标签] [选项]

  [版本标签]              形如 v0.1.0 或 0.1.0；省略时安装最新正式版本
  --version <标签>        同上（显式写法）
  --token-file <路径>     GitHub 令牌文件（默认 /etc/ctm/github-token，权限需为 0600/0400）
  --check                 只解析版本与资产并打印将要执行的命令，不下载、不安装
  -h, --help              显示本帮助

令牌优先取环境变量 GH_TOKEN / CTM_GITHUB_TOKEN，其次取令牌文件。
EOF
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"
}

# resolve_token 按优先级取令牌（不打印内容）。
resolve_token() {
  local token_file=$1
  if [[ -n ${GH_TOKEN:-} ]]; then
    printf '%s' "${GH_TOKEN}"
    return 0
  fi
  if [[ -n ${CTM_GITHUB_TOKEN:-} ]]; then
    printf '%s' "${CTM_GITHUB_TOKEN}"
    return 0
  fi
  [[ -e ${token_file} ]] || die "未找到 GitHub 令牌：请设置 GH_TOKEN，或创建 ${token_file}"
  [[ -r ${token_file} ]] || die "令牌文件不可读：${token_file}"
  local mode
  mode=$(stat -c '%a' "${token_file}")
  [[ ${mode} == "600" || ${mode} == "400" ]] || die "令牌文件权限必须是 0600 或 0400：${token_file}"
  local token
  token=$(<"${token_file}")
  [[ -n ${token} ]] || die "令牌文件为空：${token_file}"
  printf '%s' "${token}"
}

version=""
token_file=${DEFAULT_TOKEN_FILE}
check_only=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)
      [[ $# -ge 2 ]] || die "--version 缺少参数"
      version=$2
      shift 2
      ;;
    --token-file)
      [[ $# -ge 2 ]] || die "--token-file 缺少参数"
      token_file=$2
      shift 2
      ;;
    --check)
      check_only=1
      shift
      ;;
    -h | --help | help)
      usage
      exit 0
      ;;
    -*)
      usage >&2
      die "未知选项：$1"
      ;;
    *)
      [[ -z ${version} ]] || { usage >&2; die "只允许一个版本标签（多余参数：$1）"; }
      version=$1
      shift
      ;;
  esac
done

if [[ -n ${version} && ${version} != "latest" ]]; then
  [[ ${version} == v* ]] || version="v${version}"
  [[ ${version} =~ ${TAG_PATTERN} ]] || die "版本标签格式非法（只接受 v1.2.3 形式）：${version}"
  release_endpoint="tags/${version}"
else
  version=""
  release_endpoint="latest"
fi

repository=${CTM_GITHUB_REPO:-${DEFAULT_REPOSITORY}}
api_base=${CTM_GITHUB_API_BASE:-${DEFAULT_API_BASE}}
require_command curl
require_command python3
token=$(resolve_token "${token_file}")

# 参数与令牌先校验，再要求 root：--check 是只读的，非 root 也能预览
if [[ ${check_only} -eq 0 && ${EUID} -ne 0 ]]; then
  die "请使用 root 或 sudo 执行（安装需要写 /opt/ctm 与 systemd 单元）；只想预览可加 --check"
fi

TEMP_DIR=$(mktemp -d)
release_json="${TEMP_DIR}/release.json"

echo "解析发布：${repository} ${version:-最新正式版本}"
if ! curl --fail --silent --show-error --location --retry 3 \
  -H "Authorization: Bearer ${token}" \
  -H "Accept: application/vnd.github+json" \
  "${api_base}/repos/${repository}/releases/${release_endpoint}" \
  -o "${release_json}"; then
  die "读取 Release 失败：请确认令牌有效且具备该仓库的 Contents: read 权限"
fi

resolved=$(python3 - "${release_json}" <<'PY'
import json
import sys

release = json.load(open(sys.argv[1]))
assets = release.get("assets") or []
script = next((a for a in assets if a.get("name") == "install.sh"), None)
if script is None:
    sys.stderr.write("该 Release 里没有 install.sh 资产\n")
    sys.exit(3)
print(release.get("tag_name") or "")
print(script.get("url") or "")
print(script.get("name") or "install.sh")
PY
) || die "解析 Release 失败：未找到 install.sh 资产"

tag=$(printf '%s\n' "${resolved}" | sed -n '1p')
asset_url=$(printf '%s\n' "${resolved}" | sed -n '2p')
[[ -n ${tag} && -n ${asset_url} ]] || die "解析 Release 失败：缺少版本标签或资产地址"

if [[ ${check_only} -eq 1 ]]; then
  echo "目标版本：${tag}"
  echo "安装脚本：install.sh（asset id=${asset_url##*/}）"
  echo "将执行：GH_TOKEN=<已隐藏> bash install.sh install ${tag}"
  echo "（--check 只预览，不下载、不安装）"
  exit 0
fi

install_script="${TEMP_DIR}/install.sh"
echo "下载安装脚本：${tag}"
if ! curl --fail --silent --show-error --location --retry 3 \
  -H "Authorization: Bearer ${token}" \
  -H "Accept: application/octet-stream" \
  "${asset_url}" \
  -o "${install_script}"; then
  die "下载 install.sh 失败：${asset_url}"
fi
chmod 0755 "${install_script}"
first_line=$(head -n 1 "${install_script}")
if [[ ${first_line} != "#!"* ]] || ! grep -q 'install_release' "${install_script}"; then
  die "下载到的 install.sh 内容异常，已中止"
fi

# 交给 install.sh 完成下载、校验、切换、健康检查与回滚；令牌用环境变量传递，避免写入磁盘
export GH_TOKEN="${token}"
echo "开始安装：${tag}"
bash "${install_script}" install "${tag}"
