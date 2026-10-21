#!/usr/bin/env bash
# ctm 中国电信话费流量监控 —— 安装 / 更新 / 回滚
#
# 设计要点（与 yzfExchange 保持一致）：
#   - 二进制放在 /opt/ctm/releases/<tag>/，用 /opt/ctm/current 软链接切换
#   - 数据与凭据在 /var/lib/ctm，服务用户 ctm 可写，其余路径只读
#   - systemd 沙箱：NoNewPrivileges + ProtectSystem=full + ReadWritePaths=/var/lib/ctm
#   - 一键更新：服务只写请求文件，由 root 的 ctm-update.service 在沙箱外执行本脚本
#   - 健康检查失败自动回滚到上一版本

set -Eeuo pipefail

readonly REPOSITORY="MADAO-NW/ChinaTelecomAssistant"
readonly API_ROOT="https://api.github.com/repos/${REPOSITORY}"
readonly API_VERSION="2026-03-10"
readonly APP_USER="ctm"
readonly APP_GROUP="ctm"
readonly APP_ROOT="/opt/ctm"
readonly RELEASES_DIR="${APP_ROOT}/releases"
readonly CURRENT_LINK="${APP_ROOT}/current"
readonly PREVIOUS_LINK="${APP_ROOT}/previous"
readonly DATA_DIR="/var/lib/ctm"
readonly SERVICE_NAME="ctm.service"
readonly SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}"
readonly ENV_FILE="/etc/ctm/ctm.env"
readonly TOKEN_FILE="/etc/ctm/github-token"
readonly LISTEN_ADDR="0.0.0.0:9035"
readonly UPDATE_DIR="/usr/local/lib/ctm"
readonly UPDATE_RUNNER="${UPDATE_DIR}/ctm-update"
readonly UPDATE_LOG="/var/log/ctm-update.log"
readonly UPDATE_SERVICE="ctm-update.service"
readonly UPDATE_PATH_UNIT="ctm-update.path"
readonly UPDATE_SERVICE_FILE="/etc/systemd/system/${UPDATE_SERVICE}"
readonly UPDATE_PATH_FILE="/etc/systemd/system/${UPDATE_PATH_UNIT}"
readonly INSTALLED_SCRIPT="${APP_ROOT}/install.sh"

TEMP_DIR=""

cleanup() {
  if [[ -n ${TEMP_DIR} && -d ${TEMP_DIR} ]]; then
    rm -rf -- "${TEMP_DIR}"
  fi
}
trap cleanup EXIT

usage() {
  cat <<'EOF'
用法：
  sudo ./install.sh install [版本标签]
  sudo ./install.sh update [版本标签]
  sudo ./install.sh status
  sudo ./install.sh rollback

未指定版本标签时安装最新 Release。私有仓库下载令牌按以下优先级读取：
  1. 当前进程的 GH_TOKEN 环境变量
  2. /etc/ctm/github-token 文件（权限 0600/0400）

令牌只需要目标私有仓库的 Contents: read 权限。

安装或更新还会顺带安装“一键自更新”入口（都在 root 名下，服务用户不可写）：
  /usr/local/lib/ctm/ctm-update          执行器：读取请求文件并调用本脚本以 root 更新
  /etc/systemd/system/ctm-update.path    监听 /var/lib/ctm/update-request 请求文件
  /etc/systemd/system/ctm-update.service 由 path 单元触发的一次性 root 服务
管理端“一键更新”按钮只写请求文件。服务的 systemd 沙箱不允许它提权、改 /etc 或 /opt/ctm，
也不允许它重启自己所在的 cgroup，所以真正的更新由沙箱之外的 root 单元执行。
EOF
}

die() {
  echo "错误：$*" >&2
  exit 1
}

require_root() {
  [[ ${EUID} -eq 0 ]] || die "请使用 root 或 sudo 执行"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"
}

detect_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo "amd64" ;;
    aarch64 | arm64) echo "arm64" ;;
    *) die "不支持的系统架构：$(uname -m)" ;;
  esac
}

read_github_token() {
  if [[ -n ${GH_TOKEN:-} ]]; then
    printf '%s' "${GH_TOKEN}"
    return
  fi
  if [[ -r ${TOKEN_FILE} ]]; then
    local mode token
    mode=$(stat -c '%a' "${TOKEN_FILE}")
    [[ ${mode} == "600" || ${mode} == "400" ]] || \
      die "令牌文件权限必须是 0600 或 0400：${TOKEN_FILE}"
    token=$(<"${TOKEN_FILE}")
    [[ -n ${token} ]] || die "令牌文件为空：${TOKEN_FILE}"
    printf '%s' "${token}"
    return
  fi
  die "未找到 GitHub 令牌，请设置 GH_TOKEN 或创建 ${TOKEN_FILE}"
}

github_request() {
  local token=$1 accept=$2 url=$3 output=$4
  {
    printf 'header = "Authorization: Bearer %s"\n' "${token}"
    printf 'header = "Accept: %s"\n' "${accept}"
    printf 'header = "X-GitHub-Api-Version: %s"\n' "${API_VERSION}"
  } | curl \
    --config - \
    --fail \
    --silent \
    --show-error \
    --location \
    --output "${output}" \
    "${url}"
}

ensure_system_user() {
  if ! getent group "${APP_GROUP}" >/dev/null 2>&1; then
    groupadd --system "${APP_GROUP}"
  fi
  if ! id -u "${APP_USER}" >/dev/null 2>&1; then
    local nologin_shell
    nologin_shell=$(command -v nologin || true)
    [[ -n ${nologin_shell} ]] || nologin_shell="/usr/sbin/nologin"
    useradd \
      --system \
      --gid "${APP_GROUP}" \
      --home-dir "${DATA_DIR}" \
      --shell "${nologin_shell}" \
      "${APP_USER}"
  fi
}

# ensure_administrator_credentials 首次安装时自动生成管理员账号与随机密码。
# 密码只在安装结束时打印一次，之后可用 `ctm admin reset` 重新生成。
ensure_administrator_credentials() {
  local target=$1
  local credentials_file="${DATA_DIR}/auth/admin.json"
  install -d -o "${APP_USER}" -g "${APP_GROUP}" -m 0700 "${DATA_DIR}/auth"
  if [[ ! -f ${credentials_file} ]]; then
    echo "首次启用管理端登录，正在生成管理员账号与随机密码…"
    # 固定用户名 admin，密码交给程序随机生成并打印一次
    runuser -u "${APP_USER}" -- "${target}/ctm" -data-dir "${DATA_DIR}" admin init --auto \
      | tee "${DATA_DIR}/.admin-init-output"
    ADMIN_USERNAME=$(sed -n 's/^username=//p' "${DATA_DIR}/.admin-init-output" | head -n1)
    ADMIN_PASSWORD=$(sed -n 's/^password=//p' "${DATA_DIR}/.admin-init-output" | head -n1)
    rm -f "${DATA_DIR}/.admin-init-output"
  fi
  chown -R "${APP_USER}:${APP_GROUP}" "${DATA_DIR}/auth" "${DATA_DIR}/logs" 2>/dev/null || true
  chmod 0700 "${DATA_DIR}/auth"
  [[ -f ${credentials_file} ]] && chmod 0600 "${credentials_file}"
  runuser -u "${APP_USER}" -- "${target}/ctm" -data-dir "${DATA_DIR}" admin check >/dev/null 2>&1 || \
    die "目标版本无法读取现有管理员凭据，拒绝切换：${target}"
}

# print_administrator_credentials 首次安装时把随机凭据打印到终端（只显示一次）。
print_administrator_credentials() {
  if [[ -n ${ADMIN_USERNAME:-} && -n ${ADMIN_PASSWORD:-} ]]; then
    cat <<EOF

================ 管理员登录信息（只显示这一次，请立即保存）================
  用户名：${ADMIN_USERNAME}
  密　码：${ADMIN_PASSWORD}
==========================================================================
忘记密码可在服务器上执行：ctm -data-dir ${DATA_DIR} admin reset --auto
EOF
  fi
}

write_service_file() {
  local temporary_file="${SERVICE_FILE}.tmp.$$"
  install -o root -g root -m 0644 /dev/null "${temporary_file}"
  cat >"${temporary_file}" <<EOF
[Unit]
Description=中国电信话费流量监控管理台
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=${APP_USER}
Group=${APP_GROUP}
Environment=HOME=${DATA_DIR}
EnvironmentFile=-${ENV_FILE}
WorkingDirectory=${DATA_DIR}
ExecStart=${CURRENT_LINK}/ctm -data-dir ${DATA_DIR} web -listen ${LISTEN_ADDR}
Restart=on-failure
RestartSec=5s
KillMode=control-group
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ReadWritePaths=${DATA_DIR}

[Install]
WantedBy=multi-user.target
EOF
  mv -f "${temporary_file}" "${SERVICE_FILE}"
}

# write_service_env_file 把只读令牌以环境变量形式提供给服务进程（管理端“检查更新”用）。
# 令牌文件本身仍是 0600/0400 root，systemd 以 root 读取 EnvironmentFile 后注入，无需放宽权限。
write_service_env_file() {
  local token=""
  if [[ -n ${GH_TOKEN:-} ]]; then
    token=${GH_TOKEN}
  elif [[ -r ${TOKEN_FILE} ]]; then
    token=$(<"${TOKEN_FILE}")
  fi
  if [[ -z ${token} ]]; then
    echo "未找到 GitHub 令牌，跳过 ${ENV_FILE}（管理端“检查更新”会提示未配置令牌）" >&2
    return 0
  fi
  install -d -o root -g root -m 0755 "$(dirname "${ENV_FILE}")"
  install -o root -g root -m 0600 /dev/null "${ENV_FILE}"
  printf 'CTM_GITHUB_TOKEN=%s\n' "${token}" >"${ENV_FILE}"
}

# install_update_helper 安装一键更新入口（全部由 root 拥有，服务用户不可写）。
install_update_helper() {
  install -d -o root -g root -m 0755 "${UPDATE_DIR}"
  local script_source
  script_source=$(readlink -f "$0" 2>/dev/null || printf '%s' "$0")
  if [[ ${script_source} != "${INSTALLED_SCRIPT}" ]]; then
    install -o root -g root -m 0755 "${script_source}" "${INSTALLED_SCRIPT}"
  fi

  local temporary_runner="${UPDATE_RUNNER}.tmp.$$"
  # heredoc 不引号：${DATA_DIR} 等需要展开；执行器自身的变量与命令替换要转义。
  cat >"${temporary_runner}" <<RUNNER
#!/usr/bin/env bash
# 由 deploy/install.sh 安装：读取管理端的一键更新请求并以 root 执行一次更新。
# 由 systemd 的 ${UPDATE_PATH_UNIT} → ${UPDATE_SERVICE} 触发，运行在 ctm.service 沙箱之外。
set -uo pipefail

readonly REQUEST_FILE="${DATA_DIR}/update-request"
readonly RESULT_FILE="${DATA_DIR}/update-result.json"
readonly RESULT_LOG="${DATA_DIR}/update-result.log"
readonly INSTALL_SCRIPT="${INSTALLED_SCRIPT}"
readonly UPDATE_LOG="${UPDATE_LOG}"
readonly TAG_PATTERN='^v[0-9]+\.[0-9]+\.[0-9]+$'

tag=""
result_written=0

log() {
  printf '%s %s\n' "\$(date '+%Y-%m-%d %H:%M:%S')" "\$*" >>"\${UPDATE_LOG}"
}

write_result() {
  local code=\$1
  printf '{"target":"%s","exitCode":%s,"finishedAt":%s}\n' \
    "\${tag#v}" "\${code}" "\$(date +%s)000" >"\${RESULT_FILE}" 2>/dev/null || true
  chmod 0644 "\${RESULT_FILE}" "\${RESULT_LOG}" 2>/dev/null || true
  result_written=1
}

on_exit() {
  local code=\$?
  if [[ \${result_written} -eq 0 ]]; then
    log "执行异常退出（退出码 \${code}）"
    write_result "\${code}"
  fi
}
trap on_exit EXIT

if [[ -r \${REQUEST_FILE} ]]; then
  tag=\$(tr -d '\r\n' <"\${REQUEST_FILE}" 2>/dev/null || true)
fi
# 先删请求文件：避免 PathExists 反复触发，也避免同一请求被执行两次
rm -f "\${REQUEST_FILE}"
: >"\${RESULT_LOG}"

if [[ ! \${tag} =~ \${TAG_PATTERN} ]]; then
  log "拒绝执行：更新请求缺失或标签非法（\${tag}）"
  echo "更新请求缺失或标签非法：\${tag}" >>"\${RESULT_LOG}"
  write_result 64
  exit 64
fi

if [[ ! -x \${INSTALL_SCRIPT} ]]; then
  log "拒绝执行：缺少可执行的安装脚本 \${INSTALL_SCRIPT}"
  echo "缺少可执行的安装脚本：\${INSTALL_SCRIPT}" >>"\${RESULT_LOG}"
  write_result 66
  exit 66
fi

log "开始更新到 \${tag}"
status=0
"\${INSTALL_SCRIPT}" update "\${tag}" >>"\${RESULT_LOG}" 2>&1 || status=\$?
cat "\${RESULT_LOG}" >>"\${UPDATE_LOG}"
log "更新结束：\${tag}，退出码 \${status}"
write_result "\${status}"
exit "\${status}"
RUNNER
  chmod 0755 "${temporary_runner}"
  mv -f "${temporary_runner}" "${UPDATE_RUNNER}"

  local temporary_service="${UPDATE_SERVICE_FILE}.tmp.$$"
  cat >"${temporary_service}" <<EOF
[Unit]
Description=电信监控管理端一键更新（root 执行，脱离 ctm.service 沙箱）

[Service]
Type=oneshot
ExecStart=${UPDATE_RUNNER}
EOF
  chmod 0644 "${temporary_service}"
  mv -f "${temporary_service}" "${UPDATE_SERVICE_FILE}"

  local temporary_path="${UPDATE_PATH_FILE}.tmp.$$"
  cat >"${temporary_path}" <<EOF
[Unit]
Description=监听管理端的一键更新请求

[Path]
PathExists=${DATA_DIR}/update-request
Unit=${UPDATE_SERVICE}

[Install]
WantedBy=multi-user.target
EOF
  chmod 0644 "${temporary_path}"
  mv -f "${temporary_path}" "${UPDATE_PATH_FILE}"

  echo "一键自更新入口已安装：${UPDATE_RUNNER}（systemd: ${UPDATE_SERVICE} / ${UPDATE_PATH_UNIT}）"
}

switch_current_release() {
  local target=$1
  local temporary_link="${APP_ROOT}/.current.$$"
  ln -s "${target}" "${temporary_link}"
  mv -Tf "${temporary_link}" "${CURRENT_LINK}"
}

wait_for_health() {
  local attempt
  for attempt in $(seq 1 30); do
    # 未登录时管理端仍会返回 200（登录页），因此只要能响应就算健康
    if curl --fail --silent --show-error "http://127.0.0.1:9035/" >/dev/null; then
      return 0
    fi
    sleep 1
  done
  return 1
}

show_status() {
  systemctl status "${SERVICE_NAME}" --no-pager || true
  if curl --fail --silent --show-error "http://127.0.0.1:9035/" >/dev/null; then
    echo "管理端正常：http://127.0.0.1:9035/"
  else
    echo "管理端不可用：http://127.0.0.1:9035/" >&2
    return 1
  fi
  if systemctl is-active "${UPDATE_PATH_UNIT}" >/dev/null 2>&1; then
    echo "一键更新监听正常：${UPDATE_PATH_UNIT}"
  else
    echo "一键更新监听未运行：${UPDATE_PATH_UNIT}" >&2
  fi
}

install_release() {
  local requested_version=${1:-latest}
  local arch token release_url release_json
  local -a metadata

  require_root
  for command_name in curl python3 tar sha256sum systemctl install getent groupadd useradd stat runuser; do
    require_command "${command_name}"
  done

  arch=$(detect_arch)
  token=$(read_github_token)
  if [[ ${requested_version} == "latest" ]]; then
    release_url="${API_ROOT}/releases/latest"
  else
    [[ ${requested_version} =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || \
      die "版本标签格式不正确：${requested_version}"
    release_url="${API_ROOT}/releases/tags/${requested_version}"
  fi

  TEMP_DIR=$(mktemp -d /tmp/ctm-install.XXXXXX)
  release_json="${TEMP_DIR}/release.json"
  github_request "${token}" "application/vnd.github+json" "${release_url}" "${release_json}"

  mapfile -t metadata < <(
    python3 - "${release_json}" "${arch}" <<'PY'
import json
import re
import sys

release_file, arch = sys.argv[1:]
with open(release_file, "r", encoding="utf-8") as stream:
    release = json.load(stream)

tag = release.get("tag_name", "")
if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", tag):
    raise SystemExit("Release 标签格式不安全")

package_name = f"ctm-{tag}-linux-{arch}.tar.gz"
assets = {asset.get("name"): asset.get("id") for asset in release.get("assets", [])}
package_id = assets.get(package_name)
checksums_id = assets.get("SHA256SUMS")
if not package_id or not checksums_id:
    raise SystemExit(f"Release 缺少 {package_name} 或 SHA256SUMS")

print(tag)
print(package_name)
print(package_id)
print(checksums_id)
PY
  )
  [[ ${#metadata[@]} -eq 4 ]] || die "无法解析 Release 产物"

  local tag=${metadata[0]}
  local package_name=${metadata[1]}
  local package_id=${metadata[2]}
  local checksums_id=${metadata[3]}
  local package_file="${TEMP_DIR}/${package_name}"
  local checksums_file="${TEMP_DIR}/SHA256SUMS"

  github_request "${token}" "application/octet-stream" \
    "${API_ROOT}/releases/assets/${package_id}" "${package_file}"
  github_request "${token}" "application/octet-stream" \
    "${API_ROOT}/releases/assets/${checksums_id}" "${checksums_file}"

  local expected_checksum
  expected_checksum=$(awk -v name="${package_name}" '$2 == name { print $1 }' "${checksums_file}")
  [[ -n ${expected_checksum} ]] || die "SHA256SUMS 中没有 ${package_name}"
  (
    cd "${TEMP_DIR}"
    printf '%s  %s\n' "${expected_checksum}" "${package_name}" | sha256sum -c -
  )

  mkdir -p "${TEMP_DIR}/package"
  tar -xzf "${package_file}" -C "${TEMP_DIR}/package"
  [[ -f ${TEMP_DIR}/package/ctm ]] || die "发布包中缺少 ctm 二进制"

  ensure_system_user
  install -d -m 0755 "${APP_ROOT}" "${RELEASES_DIR}"
  install -d -o "${APP_USER}" -g "${APP_GROUP}" -m 0700 "${DATA_DIR}"
  install -d -o "${APP_USER}" -g "${APP_GROUP}" -m 0755 "${DATA_DIR}/logs"

  local release_dir="${RELEASES_DIR}/${tag}"
  if [[ -e ${release_dir} ]]; then
    [[ -x ${release_dir}/ctm ]] || die "版本目录不完整：${release_dir}"
  else
    install -d -m 0755 "${release_dir}"
    install -o root -g root -m 0755 "${TEMP_DIR}/package/ctm" "${release_dir}/ctm"
  fi

  # 引导管理员凭据：无需交互，随机生成密码并在安装结束时打印一次
  ensure_administrator_credentials "${release_dir}"

  local previous_target=""
  if [[ -L ${CURRENT_LINK} ]]; then
    previous_target=$(readlink -f "${CURRENT_LINK}" || true)
    if [[ -n ${previous_target} && ${previous_target} != "${release_dir}" ]]; then
      ln -sfn "${previous_target}" "${PREVIOUS_LINK}"
    fi
  fi

  switch_current_release "${release_dir}"
  write_service_file
  write_service_env_file
  install_update_helper
  systemctl daemon-reload
  systemctl enable "${SERVICE_NAME}" >/dev/null
  systemctl reset-failed "${UPDATE_SERVICE}" >/dev/null 2>&1 || true
  systemctl enable "${UPDATE_PATH_UNIT}" >/dev/null
  systemctl restart "${UPDATE_PATH_UNIT}"
  systemctl restart "${SERVICE_NAME}"

  if ! wait_for_health; then
    echo "新版本健康检查失败，开始恢复上一版本" >&2
    if [[ -n ${previous_target} && -x ${previous_target}/ctm ]]; then
      switch_current_release "${previous_target}"
      systemctl restart "${SERVICE_NAME}"
    else
      systemctl stop "${SERVICE_NAME}" || true
      echo "没有可回滚的上一版本，已停止服务以避免异常暴露。" >&2
    fi
    die "版本 ${tag} 启动失败"
  fi

  echo "版本 ${tag} 已安装并启动"
  echo "管理地址：http://127.0.0.1:9035/（监听 ${LISTEN_ADDR}）"
  echo "如需公网访问请配置防火墙或反向代理，并务必启用双因素认证。"
  print_administrator_credentials
}

rollback_release() {
  require_root
  require_command systemctl
  require_command curl
  require_command runuser

  [[ -L ${PREVIOUS_LINK} ]] || die "没有可回滚的上一版本"
  local previous_target current_target
  previous_target=$(readlink -f "${PREVIOUS_LINK}")
  [[ -x ${previous_target}/ctm ]] || die "上一版本不完整：${previous_target}"
  runuser -u "${APP_USER}" -- "${previous_target}/ctm" -data-dir "${DATA_DIR}" admin check >/dev/null 2>&1 || \
    die "上一版本无法读取现有管理员凭据，拒绝回滚：${previous_target}"
  current_target=$(readlink -f "${CURRENT_LINK}" || true)

  switch_current_release "${previous_target}"
  if [[ -n ${current_target} && -x ${current_target}/ctm ]]; then
    ln -sfn "${current_target}" "${PREVIOUS_LINK}"
  fi
  systemctl restart "${SERVICE_NAME}"
  wait_for_health || die "回滚后的版本未通过健康检查"
  echo "已回滚到：${previous_target}"
}

main() {
  local command=${1:-}
  case "${command}" in
    install | update)
      local requested_version=""
      shift
      for arg in "$@"; do
        case "${arg}" in
          -h | --help) usage; exit 0 ;;
          *)
            if [[ -z ${requested_version} ]]; then
              requested_version="${arg}"
            else
              usage >&2
              exit 1
            fi
            ;;
        esac
      done
      install_release "${requested_version:-latest}"
      ;;
    status) show_status ;;
    rollback) rollback_release ;;
    -h | --help | help) usage ;;
    *) usage >&2; exit 1 ;;
  esac
}

main "$@"
