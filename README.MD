# ECS 控制台

一个简单的阿里云 ECS 管理面板，可以查看实例状态和流量，并在流量达到阈值时自动关机，避免产生额外费用。

> 阿里云流量数据可能存在延迟，自动关机不能保证绝对不会产生费用。

## 部署

准备一台使用 systemd 或 OpenRC 的 Linux 服务器。如果使用域名，请先将域名解析到服务器 IP。当前支持 AMD64 和 ARM64。

在服务器上运行：

```bash
curl -fsSL https://raw.githubusercontent.com/elunez/ecs-controller/main/install.sh \
  | sudo env ECS_COOKIE_SECURE=1 sh
```

安装脚本会自动识别处理器架构，从 GitHub Release 下载并校验预构建程序，然后创建系统服务并启动。服务器不需要安装 Docker、Git 或 Go。

`ECS_COOKIE_SECURE=1` 表示登录 Cookie 只允许通过 HTTPS 传输。使用域名、反向代理或 Cloudflare 并由浏览器通过 HTTPS 访问时，建议保持为 `1`；反向代理到程序内部使用 HTTP 不受影响。如果直接使用 HTTP 访问，请不要设置该变量：

```bash
curl -fsSL https://raw.githubusercontent.com/elunez/ecs-controller/main/install.sh \
  | sudo sh
```

HTTP 环境错误设置为 `1` 时，可能出现登录成功后立即变成未登录。安装时的设置会写入 `/etc/ecs-controller/ecs-controller.env`。

### 安装目录

- 稳定的程序入口：`/opt/ecs-controller/current/ecs-controller`
- 实际版本文件：`/opt/ecs-controller/releases/<提交 SHA>/`
- 后台更新程序：`/opt/ecs-controller/updater.sh`
- SQLite 数据库：`/var/lib/ecs-controller/data.sqlite`
- 凭据加密密钥：`/var/lib/ecs-controller/.secret_encryption.key`
- 在线更新状态：`/var/lib/ecs-controller/update/`
- 运行配置：`/etc/ecs-controller/ecs-controller.env`

备份时必须同时备份 `data.sqlite` 和 `.secret_encryption.key`，否则数据库中的 AccessKey Secret 等加密内容可能无法恢复。

### 服务管理

使用 systemd 的 Debian、Ubuntu、CentOS、Rocky Linux 等系统：

```bash
sudo systemctl restart ecs-controller
sudo systemctl status ecs-controller
sudo systemctl stop ecs-controller
sudo systemctl start ecs-controller
sudo journalctl -u ecs-controller -f
```

使用 OpenRC 的 Alpine Linux：

```bash
sudo rc-service ecs-controller restart
sudo rc-service ecs-controller status
sudo rc-service ecs-controller stop
sudo rc-service ecs-controller start
sudo tail -f /var/log/ecs-controller.log
```

修改 `/etc/ecs-controller/ecs-controller.env` 后，需要执行对应系统的重启命令才能生效。后台在线更新服务名为 `ecs-controller-updater`，一般不需要手动操作。

服务默认只监听服务器本机的 `43211` 端口，请通过你现有的 HTTPS 访问方式打开面板，不要直接开放这个端口。

公网使用 Passkey 时必须使用 HTTPS。

### 本地测试

在自己的电脑上测试时，需要安装 Go 1.26 或允许 Go 自动下载对应工具链。在项目根目录执行：

```bash
./run.sh
```

脚本会先检查并停止当前端口上由本项目启动的旧进程，再启动最新源码。启动后访问 `http://127.0.0.1:43211`。本地数据保存在 `.local/data`，本地源码模式不启用后台在线更新；阿里云相关功能需要在界面中配置账号凭据后才能调用。

## 第一次使用

1. 打开网页，设置管理员用户名和密码，密码至少 6 位。
2. 进入“账号管理”，添加阿里云账号。
3. 在“系统设置”中设置流量阈值、自动关机和通知方式。

## 阿里云权限

请在阿里云 RAM 中创建一个用于本项目的用户，开启“OpenAPI 调用访问”，然后把 AccessKey ID 和 AccessKey Secret 填入账号管理。

项目会根据功能使用以下权限：

- **实例管理**：ECS
- **实例流量**：CMS
- **账号流量**：CDT
- **创建实例和 EIP**：VPC、ECS
- **消费情况**：账单服务

如果出现“CMS 不可用”或“CDT 不可用”，通常是对应的 RAM 权限没有开启。只查看和管理已有实例时，不需要开通创建实例、EIP 和账单权限。

## 主要功能

- 查看 ECS 状态、规格、IP 和流量。
- 查看 CMS 实例流量和 CDT 账号流量。
- CMS 或 CDT 任一流量达到阈值时自动关机。
- 定时开关机和抢占式实例自动恢复。
- 创建、启动、停止和释放 ECS。
- Telegram、邮件和 Webhook 通知。
- Passkey 免密码登录。
- 操作审计：记录网页、Telegram、定时任务、轮转、流量保护和后台任务的执行结果，不记录密码或云凭据。
- 费用分析：查看本月累计、实例/云盘/EIP/网络分项、月底预测和余额预计可用天数。
- 费用与余额阈值告警。费用告警只发送通知，不会自动停机。

## 在线更新

进入“关于 → 版本中心”，点击检查更新即可。

只有 GitHub Release 的对应平台安装包、SHA256 校验文件和 Ed25519 签名全部准备好时才会更新。当前已安装程序使用内置公钥验证发布签名，再校验安装包 SHA256；任一校验失败都不会切换版本。后台更新服务会下载、校验、切换版本并重启控制台；健康检查失败时会恢复更新前版本。更新成功后仅保留当前版本和一个回滚版本，并清理失败下载目录及被替换版本。部署机器不会编译程序，更新不会删除账号、配置和流量数据。

仓库发布者需要在 GitHub Actions 中配置名为 `ECS_RELEASE_SIGNING_KEY` 的 Ed25519 PEM 私钥。私钥只能保存在 GitHub Actions Secret 中；仓库和程序只保存对应公钥。

如果旧版本没有在线更新功能，重新运行一次安装命令即可。

## 安全提醒

- 不要把 AccessKey 或 AccessKey Secret 发给别人。
- 公网访问请使用 HTTPS，不要直接暴露 `43211` 端口。
- 建议使用 RAM 子账号，不要直接使用阿里云主账号密钥。

## Passkey

登录后进入“系统设置 → 全局设置”，点击“设置 Passkey”。以后可以使用指纹、面容、系统 PIN 或安全密钥登录。

公网使用 Passkey 时必须配置 HTTPS；本地测试可以使用 `http://localhost`。

## 相关链接

- 项目地址：https://github.com/elunez/ecs-controller
- 问题反馈：https://github.com/elunez/ecs-controller/issues
- 原版项目：https://github.com/Kori1c/ecs-controller
- 许可证：[MIT](./LICENSE)
