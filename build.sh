#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
output_dir=${OUTPUT_DIR:-"$script_dir/dist"}

cd "$script_dir"
mkdir -p "$output_dir"

version=${ECS_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || printf '%s' dev)}
commit=${ECS_COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || printf '%s' dev)}
build_date=${ECS_BUILD_DATE:-$(date -u '+%Y-%m-%dT%H:%M:%SZ')}

ldflags="-s -w \
-X github.com/Kori1c/ecs-controller/internal/app.Version=$version \
-X github.com/Kori1c/ecs-controller/internal/app.Commit=$commit \
-X github.com/Kori1c/ecs-controller/internal/app.BuildDate=$build_date"

build_target() {
    target_os=$1
    target_arch=$2
    extension=$3
    output_file="$output_dir/ecs-controller-${target_os}-${target_arch}${extension}"

    echo "正在构建 ${target_os}/${target_arch}..."
    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
        go build -trimpath -ldflags="$ldflags" \
        -o "$output_file" ./cmd/ecs-controller
}

build_target linux amd64 ""
build_target linux arm64 ""
build_target windows amd64 ".exe"
build_target windows arm64 ".exe"

# 当前程序运行时仍需要 template.html 和 static 目录。
cp template.html "$output_dir/template.html"
cp -R static "$output_dir/"

echo "构建完成，输出目录：$output_dir"
echo "  ecs-controller-linux-amd64"
echo "  ecs-controller-linux-arm64"
echo "  ecs-controller-windows-amd64.exe"
echo "  ecs-controller-windows-arm64.exe"
echo "  template.html"
echo "  static/"
