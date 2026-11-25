---
name: vantry-release
description: Use when packaging Vantry for Apple Silicon Macs running macOS 14 or later, building Windows x64 release packages on GitHub Actions, or publishing a Vantry GitHub Release.
---

# Vantry releases

Build macOS packages on the local Mac. The `Release` workflow builds Windows packages on a GitHub-hosted `windows-2022` runner. A macOS-only packaging request ends with the local artifacts. For a public release, assemble both platforms on the Mac and publish them together. The companion relay service for sessions and remote desktop is `linkd` (deployed separately); Vantry releases ship only `desktop` and `service` packages.

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
    --target aarch64-apple-darwin --out-dir "$release_dir/$product"
done

app="$release_dir/desktop/Vantry.app"
test "$(plutil -extract LSMinimumSystemVersion raw -o - "$app/Contents/Info.plist")" = 14.0
codesign --verify --deep --strict "$app"
hdiutil verify "$release_dir/desktop/Vantry-$version-macos-arm64.dmg"
```

Check each shipped executable with `xcrun vtool -show-build`. Its `minos` value must be 14.0 or lower. Open the packaged app with `open -n "$app" --args desktop --mock`, confirm the workbench window is visible, and quit the test instance. Keep the versioned desktop ZIP alongside the DMG for in-app updates.

## Build Windows on GitHub Actions

Pushing the release tag starts [the Windows release workflow](../../../.github/workflows/release.yml). It packages the x64 MSVC desktop and service, smoke-tests the portable desktop, and uploads the Actions artifact `release-windows-amd64`. For an existing remote tag that needs a new build, dispatch `gh workflow run release.yml -f tag="$tag"`.

Use `gh run list --workflow release.yml --json databaseId,displayTitle,status` to find the run titled `Windows release packages for $tag`. Set `run_id` to its database ID.

```bash
gh run watch "$run_id" --exit-status
gh run download "$run_id" --name release-windows-amd64 --dir "$release_dir/windows"
```

## Publish the GitHub Release

When publication is requested, generate desktop deltas against the previous public stable release before uploading the versioned files. For a re-run, set `previous` to the same base tag used originally. Both inputs are the actual distributed ZIPs. `make-delta` reconstructs the target and verifies the file hashes before publishing its output.

```bash
previous=${previous:-$(gh release view --json tagName --jq .tagName)}
test "$previous" != "$tag"
base=${previous#v}
mkdir -p "$release_dir/base"
deltas=()
for platform in darwin-arm64 windows-amd64; do
  folder=desktop
  if [[ $platform == windows-amd64 ]]; then folder=windows/desktop; fi
  asset="vantry-$base-$platform.zip"
  gh release download "$previous" --pattern "$asset" --dir "$release_dir/base" --clobber
  patch="$release_dir/vantry-$base-to-$version-$platform.delta"
  cargo run --locked -p xtask -- make-delta --from "$base" --to "$version" \
    --old "$release_dir/base/$asset" --new "$release_dir/$folder/vantry-$version-$platform.zip" --out "$patch"
  deltas+=("$patch")
done

gh release create "$tag" --verify-tag --title "Vantry $version" \
  --notes-file "docs/releases/$version.md" \
  "$release_dir/desktop/Vantry-$version-macos-arm64.dmg" \
  "$release_dir/desktop/vantry-$version-darwin-arm64.zip" \
  "$release_dir/service/vantry-service-$version-darwin-arm64.zip" \
  "$release_dir/windows/desktop/vantry-$version-windows-amd64.zip" \
  "$release_dir/windows/service/vantry-service-$version-windows-amd64.zip" \
  "${deltas[@]}"
```

If a release already exists, inspect it with `gh release view "$tag" --json isDraft,assets,url` and upload any missing files with `gh release upload`. Publish a completed draft with `gh release edit "$tag" --draft=false`. Verify the published asset names and return the release URL.
