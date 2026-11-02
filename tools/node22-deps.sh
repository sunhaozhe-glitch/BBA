#!/usr/bin/env bash
# Reproducible Node 22 install/build for the two Koishi packages.
#
#   tools/node22-deps.sh install   # clean install from lockfiles, then build src
#   tools/node22-deps.sh verify    # install + build + typecheck + package tests
#
# The stage is recreated from package.json, package-lock.json and the current
# src/ for both packages, so a repository lib/ that predates the current source
# can never satisfy a check. Versions come from the committed lockfiles only:
# this script does not upgrade anything and never installs global packages.
#
# Network policy: the local npm cache ($BBA_NPM_CACHE) is used first; a missing
# package may be fetched from the public npm registry. BBA_NODE_OFFLINE=1 makes
# the container network-none and requires a warm cache. No source, secret, group
# data or DSN leaves the staging tree.
#
# This does not start Koishi, a QQ adapter, or any production service.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODE="${1:-verify}"
case "$MODE" in
  install|verify) ;;
  *) echo "usage: tools/node22-deps.sh install|verify" >&2; exit 2 ;;
esac

NODE_IMAGE="${BBA_NODE_IMAGE:-node:22}"
STAGE="${BBA_NODE_STAGE:-$ROOT/build/node22-stage}"
NPM_CACHE="${BBA_NPM_CACHE:-$ROOT/build/node22-cache}"
PACKAGES=(character-runtime analysis-collector)

die() { echo "node22-deps: $*" >&2; exit 1; }

for pkg in "${PACKAGES[@]}"; do
  for required in package.json package-lock.json tsconfig.json tsconfig.build.json src; do
    [[ -e "$ROOT/koishi/$pkg/$required" ]] || die "missing koishi/$pkg/$required"
  done
done
command -v docker >/dev/null 2>&1 || die "docker is required"
docker info >/dev/null 2>&1 || die "docker daemon is not reachable"

# Rebuild the stage from tracked inputs only. node_modules, lib, generated data
# and stray tarballs are deliberately excluded.
rm -rf "$STAGE"
mkdir -p "$STAGE/koishi" "$NPM_CACHE"
for pkg in "${PACKAGES[@]}"; do
  mkdir -p "$STAGE/koishi/$pkg"
  tar -C "$ROOT/koishi/$pkg" -cf - \
    --exclude=./node_modules --exclude=./lib --exclude=./data --exclude=./*.tgz . |
    tar -C "$STAGE/koishi/$pkg" -xf -
done
# The collector depends on bba-character-runtime through `file:../character-runtime`;
# the stage keeps both packages in the same relative layout so the link resolves
# without NODE_PATH tricks.

cat > "$STAGE/verify-pins.cjs" <<'JS'
const fs = require('node:fs')
let failed = false
for (const pkgDir of process.argv.slice(2)) {
  const manifest = JSON.parse(fs.readFileSync(pkgDir + '/package.json', 'utf8'))
  for (const [name, want] of Object.entries({ ...manifest.dependencies, ...manifest.devDependencies })) {
    if (want.startsWith('file:')) {
      const target = fs.realpathSync(pkgDir + '/node_modules/' + name)
      console.log(`${manifest.name}: ${name} -> ${target}`)
      continue
    }
    const got = JSON.parse(fs.readFileSync(pkgDir + '/node_modules/' + name + '/package.json', 'utf8')).version
    if (got !== want) {
      console.error(`${manifest.name}: ${name} installed=${got} pinned=${want}`)
      failed = true
    }
  }
  console.log(`${manifest.name}: ${Object.keys(manifest.dependencies).length} runtime + ${Object.keys(manifest.devDependencies).length} dev dependencies match pins`)
}
process.exit(failed ? 1 : 0)
JS

docker_args=(--rm --user "$(id -u):$(id -g)"
  -e HOME=/tmp/home
  -e npm_config_cache=/npm-cache
  -e npm_config_update_notifier=false
  -e npm_config_audit=false
  -e npm_config_fund=false
  -v "$STAGE:/stage" -v "$NPM_CACHE:/npm-cache" -w /stage)
npm_args=(ci --ignore-scripts --no-audit --no-fund)
if [[ "${BBA_NODE_OFFLINE:-0}" == "1" ]]; then
  docker_args+=(--network none)
  npm_args+=(--offline)
else
  npm_args+=(--prefer-offline)
fi

steps=(
  "cd /stage/koishi/character-runtime && npm ${npm_args[*]} && npm run build"
  "cd /stage/koishi/analysis-collector && npm ${npm_args[*]} && npm run build"
)
if [[ "$MODE" == verify ]]; then
  steps+=(
    "cd /stage/koishi/character-runtime && npm run typecheck"
    "cd /stage/koishi/analysis-collector && npm run typecheck"
    "cd /stage/koishi/character-runtime && npm test"
    "cd /stage/koishi/analysis-collector && npm test"
  )
fi
steps+=(
  "node --version && npm --version"
  "node /stage/verify-pins.cjs /stage/koishi/character-runtime /stage/koishi/analysis-collector"
)

echo "node22-deps: stage=$STAGE mode=$MODE image=$NODE_IMAGE offline=${BBA_NODE_OFFLINE:-0}"
script="$(printf '%s\n' "${steps[@]}")"
docker run "${docker_args[@]}" "$NODE_IMAGE" sh -euc "$script"
echo "node22-deps: $MODE finished; stage retained at $STAGE"
