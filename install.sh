#!/bin/sh
# Install verified GitHub Release binaries; no Go, sudo or setup required.
set -eu
set -f

waldo_usage() {
    cat <<'EOF'
用法: sh install.sh [--version vX.Y.Z] [--bin-dir DIR]

默认安装最新稳定 Release 到 ~/.local/bin。
--version VERSION  指定 Release（可省略 v，支持预发布版本）
--bin-dir DIR      指定安装目录，不自动使用 sudo 或修改 PATH 配置
--help            显示帮助

也可设置 WALDO_VERSION、WALDO_INSTALL_DIR。
下载 amd64/arm64 两种产物并校验 SHA256，供跨架构远端自动安装使用。
EOF
}

waldo_fail() {
    printf 'waldo install: %s\n' "$*" >&2
    exit 1
}

waldo_curl() {
    curl -q --fail --silent --show-error --location \
        --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --connect-timeout 15 --max-time 180 --retry 3 \
        "$@"
}

waldo_download() {
    waldo_curl --output "$2" "$1" || waldo_fail "下载失败: $1"
}

waldo_checksum_entry() {
    # Whitelist exact filenames: never feed arbitrary manifest paths to -c.
    awk -v asset="$1" '
        $2 == asset || $2 == "*" asset {
            count++
            if (NF != 2 || length($1) != 64 || $1 ~ /[^0-9a-fA-F]/) bad = 1
            hash = tolower($1)
        }
        END {
            if (count != 1 || bad) exit 1
            print hash "  " asset
        }
    ' "$waldo_work/release-sums" >> "$waldo_work/SHA256SUMS" ||
        waldo_fail "SHA256SUMS 缺少唯一有效的 $1 校验项"
}

waldo_cleanup() {
    waldo_exit=$?
    trap - 0
    if [ -n "${waldo_stage:-}" ]; then rm -rf -- "$waldo_stage"; fi
    if [ -n "${waldo_work:-}" ]; then rm -rf -- "$waldo_work"; fi
    exit "$waldo_exit"
}

waldo_main() {
    waldo_version=${WALDO_VERSION:-latest}
    waldo_bin_dir=${WALDO_INSTALL_DIR:-}
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --help|-h) waldo_usage; return 0 ;;
            --version|--bin-dir)
                [ "$#" -ge 2 ] && [ -n "$2" ] || waldo_fail "$1 缺少参数"
                case "$2" in --*) waldo_fail "$1 缺少有效参数" ;; esac
                case "$1" in
                    --version) waldo_version=$2 ;;
                    --bin-dir) waldo_bin_dir=$2 ;;
                esac
                shift 2
                ;;
            --version=*) waldo_version=${1#*=}; shift ;;
            --bin-dir=*)
                waldo_bin_dir=${1#*=}
                [ -n "$waldo_bin_dir" ] || waldo_fail '--bin-dir 缺少参数'
                shift
                ;;
            *) waldo_usage >&2; waldo_fail "未知参数: $1" ;;
        esac
    done

    case "$(uname -s)" in
        Linux) waldo_os=linux ;;
        Darwin) waldo_os=darwin ;;
        *) waldo_fail '目前只支持 Linux 和 macOS' ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) waldo_arch=amd64 ;;
        aarch64|arm64) waldo_arch=arm64 ;;
        *) waldo_fail '目前只发布 amd64 和 arm64 产物' ;;
    esac
    for waldo_tool in curl awk mktemp cp chmod mv mkdir rm; do
        command -v "$waldo_tool" >/dev/null 2>&1 || waldo_fail "缺少命令: $waldo_tool"
    done
    if command -v sha256sum >/dev/null 2>&1; then
        waldo_checksum=sha256sum
    elif command -v shasum >/dev/null 2>&1; then
        waldo_checksum=shasum
    else
        waldo_fail '缺少 SHA256 校验命令：sha256sum 或 shasum'
    fi
    if [ -z "$waldo_bin_dir" ]; then
        [ -n "${HOME:-}" ] || waldo_fail '未设置 HOME，请使用 --bin-dir'
        waldo_bin_dir="$HOME/.local/bin"
    fi
    case "$waldo_bin_dir" in
        /*) ;;
        *) waldo_bin_dir="$(pwd)/$waldo_bin_dir" ;;
    esac
    waldo_work=$(mktemp -d "${TMPDIR:-/tmp}/waldo-install.XXXXXX")
    waldo_stage=''
    trap waldo_cleanup 0
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM

    if [ "$waldo_version" = latest ]; then
        # The website redirect avoids the anonymous GitHub API rate limit.
        waldo_resolved=$(waldo_curl --head --output /dev/null --write-out '%{url_effective}' \
            'https://github.com/BigLiao/waldo/releases/latest') || waldo_fail '无法解析最新 Release'
        case "$waldo_resolved" in
            https://github.com/BigLiao/waldo/releases/tag/*)
                waldo_version=${waldo_resolved#https://github.com/BigLiao/waldo/releases/tag/}
                ;;
            *) waldo_fail '最新 Release 未重定向到有效版本，请使用 --version 指定版本' ;;
        esac
    fi
    case "$waldo_version" in
        [0-9]*) waldo_version="v$waldo_version" ;;
    esac
    printf '%s\n' "$waldo_version" | awk '
        NR != 1 || $0 !~ /^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$/ { bad = 1 }
        END { exit (bad || NR != 1) }
    ' || waldo_fail '无效的 Release 版本，请使用 --version vX.Y.Z'

    # Resolve latest once, then pin every asset to this immutable tag.
    waldo_base="https://github.com/BigLiao/waldo/releases/download/$waldo_version"
    printf '下载 Waldo %s（%s %s）…\n' "$waldo_version" "$waldo_os" "$waldo_arch"
    waldo_download "$waldo_base/SHA256SUMS" "$waldo_work/release-sums"
    waldo_assets='waldo-linux-amd64 waldo-linux-arm64'
    if [ "$waldo_os" = darwin ]; then waldo_assets="$waldo_assets waldo-darwin-$waldo_arch"; fi
    for waldo_asset in $waldo_assets; do
        waldo_checksum_entry "$waldo_asset"
        waldo_download "$waldo_base/$waldo_asset" "$waldo_work/$waldo_asset"
    done
    (cd "$waldo_work" && if [ "$waldo_checksum" = shasum ]; then shasum -a 256 -c SHA256SUMS; else sha256sum -c SHA256SUMS; fi) || waldo_fail 'SHA256 校验失败，未替换已有安装'

    mkdir -p -- "$waldo_bin_dir"
    for waldo_target in waldo $waldo_assets; do
        [ ! -d "$waldo_bin_dir/$waldo_target" ] || waldo_fail "目标是目录: $waldo_bin_dir/$waldo_target"
    done
    # Stage on the destination filesystem. Renaming the main binary avoids
    # truncating a running executable and leaves shell wrappers' paths stable.
    waldo_stage=$(mktemp -d "$waldo_bin_dir/.waldo-install.XXXXXX")
    for waldo_asset in $waldo_assets; do cp "$waldo_work/$waldo_asset" "$waldo_stage/$waldo_asset"; done
    cp "$waldo_work/waldo-$waldo_os-$waldo_arch" "$waldo_stage/waldo"
    chmod 755 "$waldo_stage/waldo"
    for waldo_asset in $waldo_assets; do chmod 755 "$waldo_stage/$waldo_asset"; done
    # Execute from the destination filesystem: /tmp may be mounted noexec.
    waldo_actual=$(WALDO_CONFIG_DIR="$waldo_work/config" WALDO_DATA_DIR="$waldo_work/data" \
        "$waldo_stage/waldo" version) || waldo_fail '二进制无法在安装目录运行'
    case "$waldo_actual" in
        "${waldo_version#v}+"?*) ;;
        *) waldo_fail "二进制版本与 Release 不一致: $waldo_actual" ;;
    esac
    for waldo_target in $waldo_assets waldo; do
        [ ! -d "$waldo_bin_dir/$waldo_target" ] || waldo_fail "目标是目录: $waldo_target"
        if mv --version >/dev/null 2>&1; then
            mv -fT -- "$waldo_stage/$waldo_target" "$waldo_bin_dir/$waldo_target"
        else
            mv -fh -- "$waldo_stage/$waldo_target" "$waldo_bin_dir/$waldo_target"
        fi
    done
    printf '已安装 %s → %s/waldo\n' "$waldo_actual" "$waldo_bin_dir"
    case ":${PATH:-}:" in
        *":$waldo_bin_dir:"*) ;;
        *)
            if [ "$waldo_bin_dir" = "${HOME:-}/.local/bin" ]; then
                printf '%s\n' '请加入 PATH：export PATH="$HOME/.local/bin:$PATH"'
            else
                printf '请将 %s 加入 PATH，或使用完整路径运行。\n' "$waldo_bin_dir"
            fi
            ;;
    esac
    printf '下一步：%s/waldo setup <user@host>\n' "$waldo_bin_dir"
}

waldo_main "$@"
