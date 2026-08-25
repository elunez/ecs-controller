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

程序安装在 `/opt/ecs-controller`，数据保存在 `/var/lib/ecs-controller`。服务默认只监听服务器本机的 `43211` 端口，请通过你现有的 HTTPS 访问方式打开面板，不要直接开放这个端口。

公网使用 Passkey 时必须使用 HTTPS。

### 本地测试

在自己的电脑上测试时，需要安装 Go 1.26 或允许 Go 自动下载对应工具链。在项目根目录执行：

```bash
./run-local.sh
```

启动后访问 `http://127.0.0.1:43211`。本地数据保存在 `.local/data`，本地源码模式不启用后台在线更新；阿里云相关功能需要在界面中配置账号凭据后才能调用。

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

## 在线更新

进入“关于 → 版本中心”，点击检查更新即可。

只有 GitHub Release 的对应平台安装包和 SHA256 校验文件准备好时才会更新。后台更新服务会下载、校验、切换版本并重启控制台；健康检查失败时会恢复更新前版本。部署机器不会编译程序，更新不会删除账号、配置和流量数据。

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
- 许可证：[MIT](./LICENSE)
