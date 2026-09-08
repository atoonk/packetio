#!/bin/bash
# import.sh regenerates pkg/ from upstream gVisor: the commit named in PIN,
# the patches in patches/ applied on top, trimmed to the packages netstack
# needs, import paths rewritten to this module. pkg/ is never edited by
# hand; change a patch (or PIN) and run this.
#
#   ./import.sh                 rewrite pkg/ (refuses if pkg/ has uncommitted changes)
#   ./import.sh -check          regenerate into a scratch dir and diff against pkg/
#   ./import.sh -src DIR        take upstream from a local git checkout instead of GitHub
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
module=github.com/atoonk/packetio/netstack/gvisor
upstream=https://github.com/google/gvisor
mode=write
src=""
while [ $# -gt 0 ]; do
	case $1 in
	-check) mode=check ;;
	-src) src=$2; shift ;;
	*) echo "usage: $0 [-check] [-src DIR]" >&2; exit 2 ;;
	esac
	shift
done
read -r commit _ < "$here/PIN"

if [ $mode = write ] && [ -n "$(git -C "$here" status --porcelain -uno -- pkg)" ]; then
	echo "import.sh: pkg/ has uncommitted changes; commit or discard them first" >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Upstream at the pin, plus the patches. A shallow fetch of one commit is
# enough; GitHub serves reachable commits by hash.
tree=$work/gvisor
if [ -n "$src" ]; then
	git clone -q --shared --no-checkout "$src" "$tree"
else
	git init -q "$tree"
	git -C "$tree" remote add origin "$upstream"
	git -C "$tree" fetch -q --depth 1 origin "$commit"
fi
git -C "$tree" checkout -q "$commit"
git -C "$tree" -c user.name=import -c user.email=import@localhost \
	-c commit.gpgsign=false am -q "$here"/patches/*.patch

# The package closure: what netstack imports, then what those import, under
# every build tag (go mod tidy looks at every tag, so a closure for one
# GOOS/GOARCH would leave it complaining). Tests excluded.
imports() { # imports of the non-test Go files in dir $1, gvisor packages only
	grep -h '^\s*\(_\s*\)\?"gvisor.dev/gvisor/[^"]*"' "$1"/*.go 2>/dev/null |
		sed 's#.*"gvisor.dev/gvisor/##; s#"##' | sort -u || true
}
todo=$(cd "$here/.." && grep -rho --include='*.go' --exclude-dir=gvisor "\"$module/pkg/[^\"]*\"" . |
	sed "s#\"$module/##; s#\"##" | sort -u)
pkgs=""
while [ -n "$todo" ]; do
	next=""
	for p in $todo; do
		case " $pkgs " in *" $p "*) continue ;; esac
		[ -d "$tree/$p" ] || { echo "import.sh: no package $p at the pin" >&2; exit 1; }
		pkgs="$pkgs $p"
		mkdir -p "$work/scan/$p"
		for f in "$tree/$p"/*.go; do
			case $f in *_test.go) continue ;; esac
			ln -s "$f" "$work/scan/$p/"
		done
		next="$next $(imports "$work/scan/$p")"
	done
	todo=$(echo $next | tr ' ' '\n' | sort -u)
done
pkgs=$(echo $pkgs | tr ' ' '\n' | sort)

out=$work/out
for p in $pkgs; do
	mkdir -p "$out/$p"
	for f in "$tree/$p"/*.go "$tree/$p"/*.s; do
		[ -e "$f" ] || continue
		case $f in *_test.go) continue ;; esac
		cp "$f" "$out/$p/"
	done
done
# The patches must leave the tree formatted; the path rewrite then moves
# import-block alignment, which gofmt puts back.
bad=$(gofmt -l "$out")
if [ -n "$bad" ]; then
	echo "import.sh: not gofmt-clean after the patches:" >&2
	echo "$bad" >&2
	exit 1
fi
find "$out" -name '*.go' -exec sed -i "s#gvisor\.dev/gvisor/#$module/#g" {} +
gofmt -w "$out"

case $mode in
check)
	if diff -r "$out/pkg" "$here/pkg" > "$work/diff"; then
		echo "pkg/ matches PIN + patches"
	else
		head -50 "$work/diff" >&2
		echo "import.sh: pkg/ differs from PIN + patches (see above); run ./import.sh" >&2
		exit 1
	fi
	;;
write)
	rm -rf "$here/pkg"
	mv "$out/pkg" "$here/pkg"
	echo "pkg/: $(echo "$pkgs" | wc -l) packages from $upstream@${commit:0:12} + $(ls "$here"/patches/*.patch | wc -l) patches"
	;;
esac
