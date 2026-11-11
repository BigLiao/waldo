#!/bin/sh
# Install only missing waldo runtime dependencies. This file is also embedded
# in waldo so setup works without a separate source checkout.
set -eu
set -f

role=${1:-}
case "$role" in
    a) label='A（本机）'; shift; set -- ssh bwrap "$@" ;;
    b) label='B（服务器）'; shift; set -- bwrap sshfs fusermount3 "$@" ;;
    *) printf '%s\n' '用法: install.sh a [额外命令...] | install.sh b' >&2; exit 2 ;;
esac

if [ "$(uname -s)" != Linux ]; then
    printf '%s\n' 'waldo: 自动安装目前只支持 Linux。' >&2
    exit 1
fi

required=''
missing=''
for dep do
    case "$dep" in
        ''|*[!a-zA-Z0-9_.+-]*)
            printf 'waldo: 无效的依赖命令名 %s；请手动安装并检查 profile 的 needs 配置。\n' "$dep" >&2
            exit 1
            ;;
    esac
    required="$required $dep"
    if ! command -v "$dep" >/dev/null 2>&1; then
        missing="$missing $dep"
    fi
done
if [ -z "$missing" ]; then
    printf '✓ %s依赖已齐全\n' "$label"
    exit 0
fi

manager=''
for candidate in apt-get dnf yum pacman apk; do
    if command -v "$candidate" >/dev/null 2>&1; then
        manager=$candidate
        break
    fi
done
if [ -z "$manager" ]; then
    printf 'waldo: %s缺少命令:%s；未找到支持的包管理器（apt-get/dnf/yum/pacman/apk），请手动安装后重试。\n' "$label" "$missing" >&2
    exit 1
fi

packages=''
for dep in $missing; do
    case "$dep" in
        bwrap) package=bubblewrap ;;
        ssh)
            case "$manager" in
                apt-get|apk) package=openssh-client ;;
                dnf|yum) package=openssh-clients ;;
                pacman) package=openssh ;;
            esac
            ;;
        sshfs)
            case "$manager" in
                dnf|yum) package=fuse-sshfs ;;
                apk) package=sshfs ;;
                *) package=sshfs ;;
            esac
            ;;
        fusermount3) package=fuse3 ;;
        rg) package=ripgrep ;;
        git|bash) package=$dep ;;
        *)
            printf 'waldo: 无法自动安装依赖命令 %s；请手动安装后重新运行 waldo setup。\n' "$dep" >&2
            exit 1
            ;;
    esac
    case " $packages " in
        *" $package "*) ;;
        *) packages="$packages $package" ;;
    esac
done

case "$manager" in
    apt-get) manual="sudo apt-get update && sudo apt-get install -y$packages" ;;
    dnf|yum) manual="sudo $manager install -y$packages" ;;
    pacman) manual="sudo pacman -S --needed --noconfirm$packages" ;;
    apk) manual="sudo apk add$packages" ;;
esac

uid=$(id -u)
run_root() {
    if [ "$uid" -eq 0 ]; then
        "$@"
    elif ! command -v sudo >/dev/null 2>&1; then
        printf '%s\n' 'waldo: 安装系统依赖需要 root 或 sudo；当前找不到 sudo。' >&2
        return 1
    elif [ -t 0 ] && [ -t 1 ]; then
        sudo -- "$@"
    else
        # Never consume piped input as a password or wait for a hidden prompt.
        sudo -n -- "$@"
    fi
}

printf '%s缺少命令:%s；正在安装软件包:%s\n' "$label" "$missing" "$packages"
set -- $packages
install_packages() {
    case "$manager" in
        apt-get)
            run_root apt-get update || return $?
            run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y "$@"
            ;;
        dnf|yum) run_root "$manager" install -y "$@" ;;
        pacman) run_root pacman -S --needed --noconfirm "$@" ;;
        apk) run_root apk add "$@" ;;
    esac
}
if ! install_packages "$@"; then
    printf 'waldo: %s依赖安装失败。请在该机器的终端中执行：\n  %s\n完成后重新运行 waldo setup。\n' "$label" "$manual" >&2
    exit 1
fi

missing=''
for dep in $required; do
    if ! command -v "$dep" >/dev/null 2>&1; then
        missing="$missing $dep"
    fi
done
if [ -n "$missing" ]; then
    printf 'waldo: 软件包安装后仍找不到命令:%s；请检查软件源和 PATH，再运行 waldo setup。\n' "$missing" >&2
    exit 1
fi
printf '✓ %s依赖安装完成\n' "$label"
