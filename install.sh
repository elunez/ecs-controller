#!/bin/sh
set -eu

repo=${ECS_UPDATE_REPO:-elunez/ecs-controller}
install_root=/opt/ecs-controller
data_dir=/var/lib/ecs-controller
listen_addr=${ECS_HTTP_ADDR:-127.0.0.1:43211}
health_addr=$listen_addr
case "$health_addr" in
    :*) health_addr=127.0.0.1$health_addr ;;
    0.0.0.0:*) health_addr=127.0.0.1:${health_addr#*:} ;;
    \[::\]:*) health_addr=127.0.0.1:${health_addr##*:} ;;
esac
cookie_secure=${ECS_COOKIE_SECURE:-0}
service_user=ecs-controller
config_dir=/etc/ecs-controller
env_file=$config_dir/ecs-controller.env
releases_dir=$install_root/releases
current_link=$install_root/current

if [ "$(id -u)" -ne 0 ]; then
    echo "请使用 root 权限运行，例如：curl -fsSL https://raw.githubusercontent.com/$repo/main/install.sh | sudo sh" >&2
    exit 1
fi

case $(uname -s) in
    Linux) ;;
    *) echo "当前一键安装脚本仅支持 Linux。" >&2; exit 1 ;;
esac

case $(uname -m) in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "不支持的处理器架构：$(uname -m)，当前仅支持 AMD64 和 ARM64。" >&2; exit 1 ;;
esac

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    service_manager=systemd
elif command -v rc-service >/dev/null 2>&1 && command -v rc-update >/dev/null 2>&1; then
    service_manager=openrc
else
    echo "未检测到 systemd 或 OpenRC，无法配置后台服务。" >&2
    exit 1
fi

install_dependencies() {
    missing=0
    for command_name in curl tar awk sed; do
        command -v "$command_name" >/dev/null 2>&1 || missing=1
    done
    if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
        missing=1
    fi
    [ "$missing" -eq 1 ] || return

    echo "正在安装 curl、证书和解压工具..."
    if command -v apt-get >/dev/null 2>&1; then
        apt-get update
        DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl tar coreutils
    elif command -v apk >/dev/null 2>&1; then
        apk add --no-cache ca-certificates curl tar coreutils
    elif command -v dnf >/dev/null 2>&1; then
        dnf install -y ca-certificates curl tar coreutils
    elif command -v yum >/dev/null 2>&1; then
        yum install -y ca-certificates curl tar coreutils
    else
        echo "无法自动安装依赖，请先安装 curl、ca-certificates、tar 和 sha256sum。" >&2
        exit 1
    fi
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

create_service_user() {
    if id "$service_user" >/dev/null 2>&1; then
        return
    fi
    if command -v useradd >/dev/null 2>&1; then
        useradd --system --user-group --home-dir /nonexistent --shell /usr/sbin/nologin "$service_user"
    elif command -v adduser >/dev/null 2>&1; then
        adduser -S -D -H -s /sbin/nologin "$service_user"
    else
        echo "系统缺少 useradd/adduser，无法创建服务账号。" >&2
        exit 1
    fi
}

install_systemd_services() {
    cat > /etc/systemd/system/ecs-controller.service <<'EOF'
[Unit]
Description=ECS Controller
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=ecs-controller
Group=ecs-controller
EnvironmentFile=-/etc/ecs-controller/ecs-controller.env
WorkingDirectory=/opt/ecs-controller/current
ExecStart=/opt/ecs-controller/current/ecs-controller
Restart=always
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/ecs-controller

[Install]
WantedBy=multi-user.target
EOF

    cat > /etc/systemd/system/ecs-controller-updater.service <<'EOF'
[Unit]
Description=ECS Controller Background Updater
After=network-online.target ecs-controller.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-/etc/ecs-controller/ecs-controller.env
ExecStart=/opt/ecs-controller/updater.sh
Restart=always
RestartSec=3
PrivateTmp=true
ProtectHome=true

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable ecs-controller.service ecs-controller-updater.service >/dev/null
    systemctl restart ecs-controller.service
    systemctl restart ecs-controller-updater.service
}

install_openrc_services() {
    cat > /etc/init.d/ecs-controller <<'EOF'
#!/sbin/openrc-run
name="ECS Controller"
description="ECS Controller"
command="/opt/ecs-controller/current/ecs-controller"
command_user="ecs-controller:ecs-controller"
command_background="yes"
pidfile="/run/ecs-controller.pid"
output_log="/var/log/ecs-controller.log"
error_log="/var/log/ecs-controller.log"

depend() {
    need net
    after firewall
}

start_pre() {
    if [ -f /etc/ecs-controller/ecs-controller.env ]; then
        . /etc/ecs-controller/ecs-controller.env
        export TZ ECS_APP_DIR ECS_DATA_DIR ECS_HTTP_ADDR ECS_COOKIE_SECURE ECS_UPDATE_DIR ECS_UPDATE_REPO
    fi
    checkpath --directory --owner ecs-controller:ecs-controller --mode 0700 /var/lib/ecs-controller
}
EOF

    cat > /etc/init.d/ecs-controller-updater <<'EOF'
#!/sbin/openrc-run
name="ECS Controller Background Updater"
description="ECS Controller Background Updater"
command="/opt/ecs-controller/updater.sh"
command_background="yes"
pidfile="/run/ecs-controller-updater.pid"
output_log="/var/log/ecs-controller-updater.log"
error_log="/var/log/ecs-controller-updater.log"

depend() {
    need net
    after ecs-controller
}

start_pre() {
    if [ -f /etc/ecs-controller/ecs-controller.env ]; then
        . /etc/ecs-controller/ecs-controller.env
        export TZ ECS_APP_DIR ECS_DATA_DIR ECS_HTTP_ADDR ECS_COOKIE_SECURE ECS_UPDATE_DIR ECS_UPDATE_REPO
    fi
}
EOF

    chmod 0755 /etc/init.d/ecs-controller /etc/init.d/ecs-controller-updater
    rc-update add ecs-controller default >/dev/null
    rc-update add ecs-controller-updater default >/dev/null
    rc-service ecs-controller restart
    rc-service ecs-controller-updater restart
}

wait_for_health() {
    attempt=0
    while [ "$attempt" -lt 30 ]; do
        if curl -fsS --max-time 3 "http://$health_addr/healthz" >/dev/null 2>&1; then
            return 0
        fi
        attempt=$((attempt + 1))
        sleep 2
    done
    return 1
}

install_dependencies

echo "正在查询 GitHub 最新版本..."
release_json=$(curl -fsSL --retry 3 --connect-timeout 10 --max-time 60 \
    "https://api.github.com/repos/$repo/releases/latest")
version=$(printf '%s\n' "$release_json" | sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
if ! printf '%s' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "GitHub 最新发布版本无效：${version:-未找到版本}" >&2
    exit 1
fi

asset=ecs-controller-linux-$arch.tar.gz
base_url=https://github.com/$repo/releases/download/$version
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT INT TERM
archive=$work_dir/$asset
checksums=$work_dir/checksums.txt
extracted=$work_dir/release

echo "正在下载 $version（Linux/$arch）..."
curl -fL --retry 3 --connect-timeout 10 --max-time 300 -o "$archive" "$base_url/$asset"
curl -fL --retry 3 --connect-timeout 10 --max-time 60 -o "$checksums" "$base_url/checksums.txt"

expected=$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1; exit }' "$checksums")
actual=$(sha256_file "$archive")
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
    echo "安装包 SHA256 校验失败，安装已停止。" >&2
    exit 1
fi

mkdir -p "$extracted"
tar -xzf "$archive" -C "$extracted"
if [ ! -x "$extracted/ecs-controller" ] || [ ! -f "$extracted/template.html" ] || \
   [ ! -d "$extracted/static" ] || [ ! -f "$extracted/updater.sh" ]; then
    echo "安装包内容不完整，安装已停止。" >&2
    exit 1
fi

commit=$(ECS_APP_DIR="$extracted" "$extracted/ecs-controller" --version | sed -n 's/^commit=//p' | head -n 1)
case "$commit" in
    [0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F]) ;;
    *) echo "安装包没有有效的提交版本。" >&2; exit 1 ;;
esac

create_service_user
mkdir -p "$releases_dir" "$data_dir/update" "$config_dir"
chown -R "$service_user:$service_user" "$data_dir"
chmod 0700 "$data_dir" "$data_dir/update"

release_dir=$releases_dir/$commit
if [ -e "$release_dir" ]; then
    mv "$release_dir" "$releases_dir/.replaced-${commit}-$(date -u +%s)"
fi
mv "$extracted" "$release_dir"
chmod 0755 "$release_dir/ecs-controller" "$release_dir/updater.sh"
install -m 0755 "$release_dir/updater.sh" "$install_root/updater.sh"

previous_release=$(readlink "$current_link" 2>/dev/null || true)
temporary_link=$install_root/.current.$$
ln -s "$release_dir" "$temporary_link"
mv -Tf "$temporary_link" "$current_link"

if [ ! -f "$env_file" ]; then
    cat > "$env_file" <<EOF
TZ=Asia/Shanghai
ECS_APP_DIR=$current_link
ECS_DATA_DIR=$data_dir
ECS_HTTP_ADDR=$listen_addr
ECS_COOKIE_SECURE=$cookie_secure
ECS_UPDATE_DIR=$data_dir/update
ECS_UPDATE_REPO=$repo
EOF
    chmod 0640 "$env_file"
fi

echo "正在配置 $service_manager 后台服务..."
if [ "$service_manager" = systemd ]; then
    install_systemd_services
else
    install_openrc_services
fi

if ! wait_for_health; then
    echo "新版本启动失败。" >&2
    if [ -n "$previous_release" ]; then
        rollback_link=$install_root/.rollback.$$
        ln -s "$previous_release" "$rollback_link"
        mv -Tf "$rollback_link" "$current_link"
        if [ "$service_manager" = systemd ]; then
            systemctl restart ecs-controller.service || true
        else
            rc-service ecs-controller restart || true
        fi
        echo "已恢复安装前版本。" >&2
    fi
    exit 1
fi

echo "ECS 控制台安装完成：$version"
echo "访问地址：http://$listen_addr"
echo "数据目录：$data_dir"
echo "在线更新：已启用"
