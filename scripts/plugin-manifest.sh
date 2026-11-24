#!/usr/bin/env bash
# Write <name>.plugin.json for one ebac plugin, in the plugin dir (the current directory), at BUILD time.
# usage: plugin-manifest.sh NAME SOURCE[,SOURCE...]   (the exe must already be at bin/ebac-NAME)
# Format: internal/pluginrun/manifest.go (Manifest). The core refuses the plugin unless the exe's sha256 matches.
set -euo pipefail
name=${1:?name}; sources=${2:?sources}
exe=bin/ebac-$name
[ -f "$exe" ] || { echo "plugin-manifest: no $exe" >&2; exit 1; }
sha=$(sha256sum "$exe" | cut -d' ' -f1)
src=$(printf '%s' "$sources" | awk -F, '{for(i=1;i<=NF;i++) printf "%s\"%s\"", (i>1?",":""), $i}')
printf '{"name":"%s","exe":"%s","sha256":"%s","protocol":{"major":1,"minor":1},"sources":[%s]}\n' "$name" "$exe" "$sha" "$src" > "$name.plugin.json"
