#!/bin/sh
set -u

repo=${ECS_UPDATE_REPO:-elunez/ecs-controller}
install_root=${ECS_INSTALL_ROOT:-/opt/ecs-controller}
data_dir=${ECS_DATA_DIR:-/var/lib/ecs-controller}
update_dir=${ECS_UPDATE_DIR:-$data_dir/update}
service_name=${ECS_SERVICE_NAME:-ecs-controller}
health_addr=${ECS_HTTP_ADDR:-127.0.0.1:43211}
case "$health_addr" in
    :*) health_addr=127.0.0.1$health_addr ;;
    0.0.0.0:*) health_addr=127.0.0.1:${health_addr#*:} ;;
    \[::\]:*) health_addr=127.0.0.1:${health_addr##*:} ;;
esac
request_file=$update_dir/request.json
processing_file=$update_dir/request.processing.json
status_file=$update_dir/status.json
lock_dir=$update_dir/.lock
releases_dir=$install_root/releases
current_link=$install_root/current
update_request_id=

mkdir -p "$update_dir" "$releases_dir"
rmdir "$lock_dir" 2>/dev/null || true

json_escape() {
    printf '%s' "$1" | awk 'BEGIN { ORS="" } { gsub(/\\/, "\\\\"); gsub(/"/, "\\\""); gsub(/\r/, ""); printf "%s", $0 }'
}

write_status() {
    status=$1
    phase=$2
    message=$3
    progress=${4:-0}
    target=${5:-}
    current=${6:-}
    version=${7:-}
    now=$(date -u +%s)
    temporary=$status_file.tmp
    printf '{"status":"%s","phase":"%s","message":"%s","progress":%s,"target_commit":"%s","current_commit":"%s","target_version":"%s","request_id":"%s","updated_at":%s}\n' \
        "$(json_escape "$status")" \
        "$(json_escape "$phase")" \
        "$(json_escape "$message")" \
        "$progress" \
        "$(json_escape "$target")" \
        "$(json_escape "$current")" \
        "$(json_escape "$version")" \
        "$(json_escape "$update_request_id")" \
        "$now" >"$temporary"
    chmod 0644 "$temporary"
    mv -f "$temporary" "$status_file"
}

read_field() {
    field=$1
    file=$2
    sed -n "s/.*\"$field\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$file" | head -n 1
}

current_commit() {
    if [ -x "$current_link/ecs-controller" ]; then
        ECS_APP_DIR="$current_link" "$current_link/ecs-controller" --version 2>/dev/null | sed -n 's/^commit=//p' | head -n 1
    fi
}

restart_controller() {
    if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
        systemctl restart "$service_name"
        return
    fi
    if command -v rc-service >/dev/null 2>&1; then
        rc-service "$service_name" restart
        return
    fi
    return 1
}

wait_for_health() {
    attempt=0
    while [ "$attempt" -lt 30 ]; do
        attempt=$((attempt + 1))
        progress=$((78 + attempt / 2))
        [ "$progress" -le 94 ] || progress=94
        write_status running restarting "新版本服务启动中，正在等待健康检查（${attempt}/30）" "$progress" "$1" "$2" "$3"
        if curl -fsS --max-time 3 "http://$health_addr/healthz" >/dev/null 2>&1; then
            return 0
        fi
        sleep 2
    done
    return 1
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

run_update() {
    target=$(read_field target_sha "$processing_file")
    version=$(read_field target_version "$processing_file")
    update_request_id=$(read_field request_id "$processing_file")
    previous_commit=$(current_commit)

    case "$target" in
        [0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F]) ;;
        *) write_status error failed "更新请求中的提交版本无效" 0 "$target" "$previous_commit" "$version"; return ;;
    esac
    if ! printf '%s' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
        write_status error failed "更新请求中的发布版本无效" 0 "$target" "$previous_commit" "$version"
        return
    fi

    case $(uname -m) in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) write_status error failed "当前 Linux 架构不受支持：$(uname -m)" 0 "$target" "$previous_commit" "$version"; return ;;
    esac

    asset=ecs-controller-linux-$arch.tar.gz
    base_url=https://github.com/$repo/releases/download/$version
    work_dir=$(mktemp -d "$update_dir/.download.XXXXXX") || {
        write_status error failed "无法创建更新临时目录" 0 "$target" "$previous_commit" "$version"
        return
    }
    archive=$work_dir/$asset
    checksums=$work_dir/checksums.txt
    extracted=$work_dir/release

    write_status running downloading "正在下载 GitHub Release" 25 "$target" "$previous_commit" "$version"
    if ! curl -fL --retry 3 --connect-timeout 10 --max-time 300 -o "$archive" "$base_url/$asset" || \
       ! curl -fL --retry 3 --connect-timeout 10 --max-time 60 -o "$checksums" "$base_url/checksums.txt"; then
        write_status error failed "GitHub Release 下载失败，请检查网络后重试" 0 "$target" "$previous_commit" "$version"
        rm -rf "$work_dir"
        return
    fi

    expected=$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1; exit }' "$checksums")
    actual=$(sha256_file "$archive")
    if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
        write_status error failed "更新包 SHA256 校验失败，已停止更新" 0 "$target" "$previous_commit" "$version"
        rm -rf "$work_dir"
        return
    fi

    write_status running verifying "更新包校验通过，正在解压" 52 "$target" "$previous_commit" "$version"
    mkdir -p "$extracted"
    if ! tar -xzf "$archive" -C "$extracted" || \
       [ ! -x "$extracted/ecs-controller" ] || \
       [ ! -f "$extracted/template.html" ] || \
       [ ! -d "$extracted/static" ] || \
       [ ! -f "$extracted/updater.sh" ]; then
        write_status error failed "更新包内容不完整，已停止更新" 0 "$target" "$previous_commit" "$version"
        rm -rf "$work_dir"
        return
    fi

    packaged_commit=$(ECS_APP_DIR="$extracted" "$extracted/ecs-controller" --version 2>/dev/null | sed -n 's/^commit=//p' | head -n 1)
    if [ "$packaged_commit" != "$target" ]; then
        write_status error failed "更新包提交版本与目标版本不一致" 0 "$target" "$previous_commit" "$version"
        rm -rf "$work_dir"
        return
    fi

    release_dir=$releases_dir/$target
    if [ -e "$release_dir" ]; then
        mv "$release_dir" "$releases_dir/.replaced-${target}-$(date -u +%s)"
    fi
    mv "$extracted" "$release_dir"
    chmod 0755 "$release_dir/ecs-controller" "$release_dir/updater.sh"

    previous_release=$(readlink "$current_link" 2>/dev/null || true)
    temporary_link=$install_root/.current.$$
    ln -s "$release_dir" "$temporary_link"
    mv -Tf "$temporary_link" "$current_link"

    write_status running restarting "新版本已就绪，正在重启服务" 76 "$target" "$previous_commit" "$version"
    if restart_controller && wait_for_health "$target" "$previous_commit" "$version"; then
        install -m 0755 "$release_dir/updater.sh" "$install_root/updater.sh"
        write_status success completed "更新完成，当前已运行最新版本" 100 "$target" "$target" "$version"
        rm -rf "$work_dir"
        return
    fi

    if [ -n "$previous_release" ]; then
        temporary_link=$install_root/.rollback.$$
        ln -s "$previous_release" "$temporary_link"
        mv -Tf "$temporary_link" "$current_link"
        restart_controller >/dev/null 2>&1 || true
    fi
    write_status error rolled_back "新版本健康检查失败，已恢复更新前版本" 0 "$target" "$previous_commit" "$version"
    rm -rf "$work_dir"
}

while :; do
    if [ ! -f "$request_file" ] && [ -f "$processing_file" ]; then
        mv -f "$processing_file" "$request_file"
    fi
    if [ -f "$request_file" ] && mkdir "$lock_dir" 2>/dev/null; then
        mv -f "$request_file" "$processing_file"
        run_update
        rm -f "$processing_file"
        rmdir "$lock_dir" 2>/dev/null || true
    fi
    sleep 2
done
