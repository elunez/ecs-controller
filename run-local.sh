#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

ECS_APP_DIR=${ECS_APP_DIR:-$script_dir}
ECS_DATA_DIR=${ECS_DATA_DIR:-$script_dir/.local/data}
ECS_HTTP_ADDR=${ECS_HTTP_ADDR:-127.0.0.1:43211}
ECS_COOKIE_SECURE=${ECS_COOKIE_SECURE:-0}

export ECS_APP_DIR ECS_DATA_DIR ECS_HTTP_ADDR ECS_COOKIE_SECURE

mkdir -p "$ECS_DATA_DIR"
cd "$script_dir"

http_port=${ECS_HTTP_ADDR##*:}
case "$http_port" in
    ''|*[!0-9]*)
        echo "ECS_HTTP_ADDR 端口无效：$ECS_HTTP_ADDR" >&2
        exit 1
        ;;
esac

stop_existing_controller() {
    if ! command -v lsof >/dev/null 2>&1; then
        echo "未找到 lsof，无法自动检查并停止旧进程。" >&2
        exit 1
    fi

    listener_pids=$(lsof -nP -tiTCP:"$http_port" -sTCP:LISTEN 2>/dev/null || true)
    [ -n "$listener_pids" ] || return 0

    for listener_pid in $listener_pids; do
        listener_command=$(ps -p "$listener_pid" -o command= 2>/dev/null || true)
        case "$listener_command" in
            *ecs-controller*)
                echo "正在停止旧的 ECS 控制台进程（PID ${listener_pid}）..."
                kill "$listener_pid"
                ;;
            *)
                echo "端口 $http_port 已被其他程序占用：${listener_command:-PID $listener_pid}" >&2
                echo "为避免误停止其他程序，本次启动已取消。" >&2
                exit 1
                ;;
        esac
    done

    wait_count=0
    while [ "$wait_count" -lt 50 ]; do
        remaining_pids=$(lsof -nP -tiTCP:"$http_port" -sTCP:LISTEN 2>/dev/null || true)
        if [ -z "$remaining_pids" ]; then
            echo "旧的 ECS 控制台已停止。"
            return 0
        fi
        sleep 0.1
        wait_count=$((wait_count + 1))
    done

    echo "旧的 ECS 控制台未能在 5 秒内停止，本次启动已取消。" >&2
    exit 1
}

stop_existing_controller

echo "ECS 控制台将启动在 http://$ECS_HTTP_ADDR"
echo "本地数据目录：$ECS_DATA_DIR"

exec go run ./cmd/ecs-controller
