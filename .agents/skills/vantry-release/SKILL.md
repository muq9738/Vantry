---
name: vantry-release
description: Package and publish Vantry for macOS ARM64 by default. Build Windows x64 on GitHub Actions only when the user explicitly requests it; do not include Linux in routine releases.
---

# Vantry releases

Default to macOS ARM64 only, including for a public release. Build its desktop and service packages on the local Mac. Do not build or wait for Windows or Linux artifacts unless the user explicitly requests those platforms. Only an explicit Windows x64 request authorizes dispatching the `Release` workflow on a GitHub-hosted `windows-2022` runner; pushing a version tag must not trigger that workflow. A packaging-only request ends with local artifacts; a publication request continues through GitHub Release creation and verification. The companion relay service is `linkd` (deployed separately); Vantry releases ship only `desktop` and `service` packages.

## Prepare the release

Use the version in `Cargo.toml` under `[workspace.package]`. The release tag is `v<version>`. For publication, include the English release notes in `docs/releases/<version>.md` in the tagged source. Write the release title, description, and asset labels in English.

Run the shell examples from the repository root in the same Bash session. Build public packages from a clean checkout of the release source. If the development checkout contains `resources/update-token`, use a fresh worktree. The packager includes that local credential when present.

```bash
set -e
version=$(python3 -c 'import tomllib; print(tomllib.load(open("Cargo.toml", "rb"))["workspace"]["package"]["version"])')
tag="v$version"
mkdir -p dist
release_dir=$(mktemp -d "$PWD/dist/$tag.XXXXXX")
```

## Package macOS locally

Use an Apple Silicon Mac with Xcode Command Line Tools, the repository's Rust toolchain, and Python 3.11 or later. Set `VANTRY_REMOTE_PLUGIN_ROOT` to the remote-desktop plugin checkout to build the native helper. An existing release helper can also be supplied at `resources/darwin-arm64/remote-desktop/vantry-remote-helper`. For a desktop-only request, use just `desktop` in the product loop.

```bash
export MACOSX_DEPLOYMENT_TARGET=14.0
if [[ -n "${VANTRY_REMOTE_PLUGIN_ROOT:-}" ]]; then
  python3 scripts/build_remote_helper.py --platform darwin/arm64 --plugin-root "$VANTRY_REMOTE_PLUGIN_ROOT"
fi
test -x resources/darwin-arm64/remote-desktop/vantry-remote-helper

cargo test --workspace --locked
for product in desktop service; do
  cargo run --locked -p xtask -- package "$product" \
    --profile release-desktop --service-profile release-desktop \
    --target aarch64-apple-darwin --out-dir "$release_dir/$product"
done

app="$release_dir/desktop/Vantry.app"
test "$(plutil -extract LSMinimumSystemVersion raw -o - "$app/Contents/Info.plist")" = 14.0
codesign --verify --deep --strict "$app"
hdiutil verify "$release_dir/desktop/Vantry-$version-macos-arm64.dmg"
```

Use the size-optimized `release-desktop` profile for both macOS executables to keep the signed application within the packager's 50 MB limit. Do not relax the size check when a default Core profile makes the bundle too large.

Check each shipped executable with `xcrun vtool -show-build`. Its `minos` value must be 14.0 or lower. Open the packaged app with `open -n "$app" --args desktop --mock`, confirm the workbench window is visible, and quit the test instance. Keep the versioned desktop ZIP alongside the DMG for in-app updates.

## Build Windows on GitHub Actions

Skip this entire section unless the user explicitly requests Windows x64. After pushing the release tag, manually dispatch [the Windows release workflow](../../../.github/workflows/release.yml) with `gh workflow run release.yml -f tag="$tag"`. It packages the x64 MSVC desktop and service, smoke-tests the portable desktop, and uploads the Actions artifact `release-windows-amd64`. A normal tag push does not start a Windows build.

Use `gh run list --workflow release.yml --json databaseId,displayTitle,status` to find the run titled `Windows release packages for $tag`. Set `run_id` to its database ID.

```bash
gh run watch "$run_id" --exit-status
gh run download "$run_id" --name release-windows-amd64 --dir "$release_dir/windows"
```

## Publish the GitHub Release

When publication is requested, generate cross-version desktop deltas against recent published stable releases (exponential skip-list offsets `1, 2, 3, 5, 8` before `$tag`, up to 5 bases) before uploading the versioned files. This allows users who are 1, 2, 3, 5, or 8 versions behind to update in a **single** direct incremental delta, and users further behind to bridge in at most 2–3 hops via Dijkstra planning. Both inputs are the actual distributed ZIPs. Generate deltas only for platforms requested in this release. If a base release has no ZIP for an explicitly requested platform, skip that base and continue. `xtask make-deltas` builds all deltas in parallel, drops any delta that exceeds the savings threshold, and reconstructs + SHA-256-verifies every output before publishing.

```bash
# Collect skip-list base tags (offsets 0,1,2,4,7 among recent non-draft stable releases != $tag).
base_tags=($(gh release list --exclude-drafts --exclude-pre-releases --limit 20 --json tagName \
  --jq "[.[] | select(.tagName != \"$tag\") | .tagName] | [.[0], .[1], .[2], .[4], .[7]] | map(select(. != null)) | unique | .[]"))
test "${#base_tags[@]}" -gt 0
mkdir -p "$release_dir/base" "$release_dir/deltas"
# Default release; add windows-amd64 only for an explicit Windows x64 request.
platforms=(darwin-arm64)
for platform in "${platforms[@]}"; do
  folder=desktop
  if [[ $platform == windows-amd64 ]]; then folder=windows/desktop; fi
  base_args=()
  for prev_tag in "${base_tags[@]}"; do
    base=${prev_tag#v}
    asset="vantry-$base-$platform.zip"
    if gh release download "$prev_tag" --pattern "$asset" --dir "$release_dir/base" --clobber 2>/dev/null; then
      base_args+=(--base "$base=$release_dir/base/$asset")
    fi
  done
  if [[ "${#base_args[@]}" -gt 0 ]]; then
    cargo run --release --locked -p xtask -- make-deltas \
      --to "$version" --platform "$platform" \
      --new "$release_dir/$folder/vantry-$version-$platform.zip" \
      --out-dir "$release_dir/deltas" "${base_args[@]}"
  fi
done
shopt -s nullglob
deltas=("$release_dir"/deltas/*.delta)
shopt -u nullglob

assets=(
  "$release_dir/desktop/Vantry-$version-macos-arm64.dmg"
  "$release_dir/desktop/vantry-$version-darwin-arm64.zip"
  "$release_dir/service/vantry-service-$version-darwin-arm64.zip"
)
# Only for an explicit Windows x64 request, after its build and checks finish:
# assets+=("$release_dir/windows/desktop/vantry-$version-windows-amd64.zip"
#           "$release_dir/windows/service/vantry-service-$version-windows-amd64.zip")
gh release create "$tag" --verify-tag --title "Vantry $version" \
  --notes-file "docs/releases/$version.md" "${assets[@]}" "${deltas[@]}"
```

If a release already exists, inspect it with `gh release view "$tag" --json isDraft,assets,url` and upload any missing files with `gh release upload`. Publish a completed draft with `gh release edit "$tag" --draft=false`. Verify the published asset names and return the release URL.
