#!/bin/sh
# rebuild.sh - build the GuriMoon server with every module in modules/.
# Needs Go 1.24 or newer. The Linux, macOS and Docker counterpart of
# rebuild.ps1; same modes, same module discovery, same output.
#
#   ./rebuild.sh              build WITH modules -> ./gurimoon
#   ./rebuild.sh check        compile check WITH modules -> build/gurimoon-check
#   ./rebuild.sh core         module-less build -> ./gurimoon
#   ./rebuild.sh -o PATH ...  write the executable to PATH (the Dockerfile uses this)
#
# Module discovery (modules/<name>/):
#   - skeleton/, or a folder holding a file named .disabled  -> skipped
#   - has mod-rebuild.sh     -> OVERLAY module. Called with
#                               --overlay-only --server-repo <here>; it must write
#                               modules/<name>/build/overlay.json, a go build
#                               -overlay map (absolute paths) with one entry for
#                               modules/modules.go whose blank imports enable it.
#   - has only mod-rebuild.ps1 -> overlay module with no shell build: SKIPPED
#                               with a warning. Give it a mod-rebuild.sh to
#                               include it here.
#   - has .go files          -> IN-TREE module, blank-imported as
#                               gurimoon/modules/<name>.
#   - may ship build-tags.txt (one Go build tag per line, # comments); every
#     included module's tags go to go build -tags.
#
# cmd/overlaymerge merges the overlays, harvests the blank imports and stops
# the build when two modules map one file. GOOS/GOARCH/CGO_ENABLED pass
# through to go build.

set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
builddir="$root/build"
mode=""
out=""

while [ $# -gt 0 ]; do
    case "$1" in
        -o) out="$2"; shift 2 ;;
        check|core) mode="$1"; shift ;;
        "") shift ;;
        *) echo "Unknown mode '$1'. Use: (none) | check | core [-o PATH]"; exit 1 ;;
    esac
done

fail() { echo "=== BUILD FAILED: $1 ==="; exit 1; }

# add_tags <module dir> <module name>: union the module's build-tags.txt into
# $tags. Called only for a module that is included, so a skipped module's tag
# never reaches go build.
add_tags() {
    [ -f "$1/build-tags.txt" ] || return 0
    while IFS= read -r line || [ -n "$line" ]; do
        t=$(printf '%s' "$line" | tr -d ' \t\r')
        case "$t" in ""|\#*) continue ;; esac
        case "$t" in *[!A-Za-z0-9_.]*) fail "module $2 : bad build tag '$t' in build-tags.txt" ;; esac
        case ",$tags," in *",$t,"*) ;; *) tags="${tags:+$tags,}$t" ;; esac
    done < "$1/build-tags.txt"
}

# -- Module discovery ----------------------------------------------------------
included=""
imports=""
overlays=""
tags=""

if [ "$mode" != "core" ]; then
    for dir in "$root"/modules/*/; do
        [ -d "$dir" ] || continue
        name=$(basename "$dir")
        [ "$name" = "skeleton" ] && continue
        if [ -f "$dir/.disabled" ]; then
            echo "module $name : disabled (.disabled present), skipped"
            continue
        fi

        if [ -f "$dir/mod-rebuild.sh" ]; then
            echo "=== module $name : collecting overlay ==="
            sh "$dir/mod-rebuild.sh" --overlay-only --server-repo "$root" || fail "module $name overlay step"
            [ -f "$dir/build/overlay.json" ] || fail "module $name wrote no build/overlay.json"
            overlays="$overlays $dir/build/overlay.json"
            included="${included:+$included, }$name"
            add_tags "$dir" "$name"
            continue
        fi
        if [ -f "$dir/mod-rebuild.ps1" ]; then
            echo "module $name : has mod-rebuild.ps1 but no mod-rebuild.sh, SKIPPED (Windows-only module build)"
            continue
        fi
        if ls "$dir"/*.go >/dev/null 2>&1; then
            imports="$imports -import gurimoon/modules/$name"
            included="${included:+$included, }$name"
            add_tags "$dir" "$name"
            echo "module $name : in-tree, imported as gurimoon/modules/$name"
        fi
    done
fi

# -- Build ---------------------------------------------------------------------
if [ -z "$out" ]; then
    if [ "$mode" = "check" ]; then
        out="$builddir/gurimoon-check"
    else
        out="$root/gurimoon"
    fi
fi
case "$(go env GOOS)" in windows) case "$out" in *.exe) ;; *) out="$out.exe" ;; esac ;; esac

set -- build
if [ -n "$included" ]; then
    mkdir -p "$builddir"
    # shellcheck disable=SC2086
    (cd "$root" && go run ./cmd/overlaymerge -root "$root" -out "$builddir" $imports $overlays) || fail "overlay merge"
    set -- "$@" -overlay "$builddir/overlay.json"
    echo "=== Building server WITH modules: $included ==="
elif [ "$mode" = "core" ]; then
    echo "=== Building server, core only (no modules) ==="
else
    echo "=== Building server (no modules found) ==="
fi
if [ -n "$tags" ]; then
    set -- "$@" -tags "$tags"
    echo "build tags: $tags"
fi

(cd "$root" && go "$@" -o "$out" ./cmd/gurimoon/) || fail "go build"
echo "=== Build OK: $out ==="
