# ChinaTelecomAssistant

**中国电信 话费、流量、语音监控 + 带管理员登录与双因素认证的 Web 管理台**

单个二进制、单个端口，默认监听 `0.0.0.0:9035`。管理台自带登录保护与双因素认证，可部署在自己的 VPS 上长期运行。

## 功能

- **用量概览**：话费余额、通用/专用流量、语音用量，定时采集 + 手动刷新
- **流量提醒**：通用流量剩余低于设定值时发邮件，可配多个收件人；调整提醒线后仍不足会立即重发
- **邮箱配置**：填好 SMTP 即可试发，不用先保存
- **运行日志**：按日期落盘，支持日期 / 级别 / 关键词筛选与自动刷新
- **电信账号**：滑块 + 短信验证码登录，也可用手机号 + 密码
- **安全**：Argon2id 密码、TOTP 双因素、滑动 Session、CSRF 防护
- **运维**：Release 软链接切换、健康检查失败自动回滚、管理台一键更新

## 快速开始（本地）

```bash
git clone https://github.com/MADAO-NW/ChinaTelecomAssistant.git
cd ChinaTelecomAssistant

go build -o ctm ./cmd/ctm

# 1. 生成管理员账号与随机密码（只打印一次）
./ctm admin init --auto

# 2. 启动（默认 0.0.0.0:9035）
./ctm web
```

打开 `http://127.0.0.1:9035`，用上面打印的账号登录。

其它命令：

```bash
./ctm web --daemon              # 后台常驻（写 data/web.pid）
./ctm web --stop                # 停止后台实例
./ctm web -listen 127.0.0.1:9035   # 只监听本机
./ctm admin reset --auto        # 重置为新的随机密码
./ctm admin set-password        # 交互式改密码（运行中的服务需重启）
./ctm admin disable-2fa         # 忘记认证器时停用 2FA
./ctm admin check               # 校验凭据文件是否可用
./ctm version
```

数据目录优先级：`-data-dir` 参数 > `CTM_DATA_DIR` 环境变量 > 可执行文件旁 `data/`。

```
<data>/auth/admin.json          管理员凭据（Argon2id，0600）
<data>/auth/admin-sessions.json 管理员 Session（滑动 30 天）
<data>/auth/totp-state.json     TOTP 防重放时间步
<data>/session.json             电信登录态（手机号 + token）
<data>/device_id                电信设备号
<data>/notify.json              邮件通知配置与流量提醒记录（0600，含 SMTP 密码）
<data>/logs/YYYY-MM-DD.log      运行日志（保留 15 天）
```

## VPS 部署

仓库是公开的，安装全程不需要令牌。

### 一行安装（推荐）

```bash
curl -fsSL https://raw.githubusercontent.com/MADAO-NW/ChinaTelecomAssistant/main/deploy/bootstrap.sh | sudo bash -s --
```

指定版本：

```bash
curl -fsSL https://raw.githubusercontent.com/MADAO-NW/ChinaTelecomAssistant/main/deploy/bootstrap.sh | sudo bash -s -- v1.0.0
```

只预览将要安装的版本（不需要 root）：

```bash
curl -fsSL https://raw.githubusercontent.com/MADAO-NW/ChinaTelecomAssistant/main/deploy/bootstrap.sh | bash -s -- --check
```

### 手动安装

```bash
git clone https://github.com/MADAO-NW/ChinaTelecomAssistant.git
cd ChinaTelecomAssistant

sudo ./deploy/install.sh install            # 最新 Release
sudo ./deploy/install.sh install v1.0.0     # 指定版本
sudo ./deploy/install.sh status
sudo ./deploy/install.sh rollback
```

安装脚本会自动生成管理员账号与随机密码，并在结束时**打印一次**，请立即保存。忘记时在服务器上执行：

```bash
sudo -u ctm /opt/ctm/current/ctm -data-dir /var/lib/ctm admin reset --auto
```

安装后的目录布局：

```
/opt/ctm/current -> /opt/ctm/releases/<tag>   当前版本（软链接）
/opt/ctm/previous -> ...                      上一版本（回滚用）
/opt/ctm/install.sh                           已安装的安装脚本（root 所有）
/var/lib/ctm/                                 数据、凭据、日志（ctm 用户所有）
/usr/local/lib/ctm/ctm-update                 一键更新执行器（root 所有）
```

服务以专用系统用户 `ctm` 运行，systemd 沙箱为 `NoNewPrivileges` + `ProtectSystem=full` + `ReadWritePaths=/var/lib/ctm`，只能读写自己的数据目录。

### 开放端口

默认监听 `0.0.0.0:9035`。请在云厂商安全组放行 9035，并务必：

1. 装完立刻保存打印出来的管理员密码
2. 在管理台「安全设置」启用双因素认证
3. 有条件时在前置 Nginx/Caddy 上加 HTTPS 与来源限制

## 更新

### 管理台一键更新

「系统更新」页可以检查 GitHub Release 并一键更新到最新正式版本。服务本身不提权，提权路径是：

```
管理台写 /var/lib/ctm/update-request（内容 v<x.y.z>）
→ root 的 ctm-update.path 监听到文件出现
→ ctm-update.service 执行 /usr/local/lib/ctm/ctm-update
→ 调用 /opt/ctm/install.sh update v<x.y.z>
→ 结果写回 /var/lib/ctm/update-result.json 与 update-result.log
```

服务进程不能改 `/etc`、`/opt/ctm`，也不能重启自己所在的 cgroup；执行器归 root 所有，服务用户不可写。更新状态落盘，重启后按执行器结果与当前版本对账。

### 命令行更新

```bash
sudo ./deploy/install.sh update            # 更新到最新
sudo ./deploy/install.sh update v1.0.1     # 更新到指定版本
sudo ./deploy/install.sh rollback          # 回滚到上一版本
```

### 关于令牌（可选）

公开仓库匿名调用 GitHub API 的限额是**每出口 IP 每小时 60 次**，正常使用远够。如果同一 IP 反复安装碰到限流，可以提供一个只读令牌抬高限额：

```bash
sudo install -d -m 0755 /etc/ctm
sudo sh -c 'printf "%s" "<你的只读令牌>" > /etc/ctm/github-token'
sudo chmod 0600 /etc/ctm/github-token
```

写了这个文件后，安装与管理台「检查更新」都会自动带上它。令牌不是安装的前提。

## 发布新版本

```bash
git tag v1.0.0
git push origin v1.0.0
```

CI（`.github/workflows/release.yml`）会跑测试、交叉编译 `linux/amd64` 与 `linux/arm64`、生成 `SHA256SUMS`，并把 `install.sh` / `bootstrap.sh` 一起挂到 Release。安装脚本按 `ctm-<tag>-linux-<arch>.tar.gz` 找包并校验哈希。

## 接口

管理台页面同源调用，`/api/auth/*` 免认证，其余 `/api/*` 需管理员 Session，写操作还要 `X-CSRF-Token`。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/auth/status` | 登录阶段（password / totp / authenticated） |
| POST | `/api/auth/login` | 用户名 + 密码；启用 2FA 时返回 202 |
| POST | `/api/auth/2fa/verify` | 动态码，换正式 Session |
| POST | `/api/auth/logout` | 退出登录 |
| POST | `/api/auth/password` | 改密码（清空所有 Session） |
| POST | `/api/auth/username` | 改用户名（清空所有 Session） |
| POST | `/api/auth/2fa/setup` | 生成 TOTP 密钥与二维码 |
| POST | `/api/auth/2fa/enable` | 用动态码确认启用 |
| POST | `/api/auth/2fa/disable` | 密码 + 动态码停用 |
| GET | `/api/usage` | 用量快照 |
| POST | `/api/usage/refresh` | 强制刷新 |
| GET/POST | `/api/telecom/session` | 查询 / 保存 / 清除电信登录态 |
| POST | `/api/telecom/{start,challenge,verify,sms,login,password-login}` | 电信账号登录链路 |
| GET/PUT | `/api/notify` | 读取 / 保存邮件与流量提醒配置 |
| POST | `/api/notify/test` | 用当前填写内容试发一封（不保存） |
| POST | `/api/monitor/check` | 立即检测一次流量 |
| DELETE | `/api/monitor/records` | 清空提醒记录 |
| GET | `/api/logs/dates` | 可查询的日志日期列表 |
| GET | `/api/logs` | 按日期 / 级别 / 关键词查询日志 |
| GET | `/api/system/version` | 版本检查（`?force=1` 绕过缓存） |
| GET/POST | `/api/system/update` | 更新状态 / 发起一键更新 |

兼容旧版面板的公开接口（无需登录）：

```bash
curl http://127.0.0.1:9035/show/flow
```

```json
{
  "code": 200,
  "data": {
    "username": "",                  // 非 dev 模式不回显手机号
    "use": 12276406, "total": 167045874,          // 流量，单位 KB
    "generalUse": 12276406, "generalTotal": 83159794,
    "specialUse": 0, "specialTotal": 83886080,
    "balance": 7036,                 // 话费余额，单位分
    "voiceUsage": 0, "voiceAmount": 500,          // 语音，单位分钟
    "createTime": "2022-04-26 15:37:47",
    "items": [{ "name": "国内流量", "use": 12276406, "total": 83159794 }]
  }
}
```

## 启动参数

```bash
./ctm web -h
```

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-listen` | `0.0.0.0:9035` | 监听地址 |
| `-username` / `-password` | 空 | 电信账号，可选；填了则启动即登录并在 token 过期时自动重登 |
| `-dataDir`（全局） | 可执行文件旁 `data/` | 数据目录 |
| `-intervalsTime` | 180 | 用量接口防抖（秒） |
| `-timeOut` | 30 | 电信接口超时（秒） |
| `-loginIntervalTime` | 43200 | 两次密码登录最小间隔（秒，防封号） |
| `-clientVersion` | 12.2.0 | 密码登录使用的电信客户端版本 |
| `-logLevel` / `-logEncoding` | info / console | 日志等级与格式 |
| `-dev` | false | 开放上游原始响应接口 |

## 目录

```
cmd/ctm/              入口：web / admin / version 子命令
internal/admin/       管理员凭据、Session、TOTP、登录限流
internal/auth/        电信登录：滑块、短信、密码、会话持久化
internal/telecom/     业务网关：话费/流量查询与汇总换算
internal/mailer/      SMTP 发信（SSL / STARTTLS / 明文）
internal/notify/      通知配置、流量提醒监控与记录
internal/version/     构建版本注入
internal/web/         HTTP 服务、认证中间件、管理 API、内嵌页面
configs/              数据目录布局与运行时配置
models/               电信接口与用量汇总数据结构
deploy/               install.sh / bootstrap.sh
```

## 安全说明

- 密码用 Argon2id（m=19456, t=2, p=1）哈希，校验走常量时间比较
- Session 是 32 字节随机 token，HttpOnly + SameSite=Strict，滑动 30 天；改密码/改用户名/改 2FA 会让全部 Session 立即失效
- 所有写操作校验 `X-CSRF-Token`；登录失败按 IP+账号渐进延迟（3 次后 1s、2s…最多 30s）
- TOTP 校验接受当前及相邻时间步，并持久化已消费的时间步，防止动态码在重启前后重放
- 响应头带 CSP、`X-Frame-Options: DENY`、`X-Content-Type-Options: nosniff`、`no-store`
- `notify.json` 内含 SMTP 密码，落盘权限 0600，接口只回传「是否已设置」

## 说明

本项目仅供个人查询自己的电信账号用量使用。请遵守运营商服务条款，不要用于批量或商业用途。登录链路参考了 [LambdaExpression/ChinaTelecomMonitor](https://github.com/LambdaExpression/ChinaTelecomMonitor) 的公开实现。

## 许可证

MIT
