#!/bin/sh
# Prepare the pinned desktop resources, then let MyGo assemble and sign the app.
set -eu
cd "$(dirname "$0")/.."

case "$(uname -s)-$(uname -m)" in
    Darwin-arm64)
        arch=arm64
        target=aarch64-apple-darwin
        archive_sha=3750b2e93f37e0c692657da574d7019a101c0084da05a790c83fd335bad973e4
        rg_sha=a326a1fb48074202e9ad41e4cd1e389eeea372c8c6f7d7e80da81176d5d9430e
        ;;
    Darwin-x86_64)
        arch=amd64
        target=x86_64-apple-darwin
        archive_sha=af7825fcc69a2afc7a7aea55fc9af90e26421d8f20fe59df32e233c0b8a231c1
        rg_sha=0c9a0066db0d26b640777db88045b0ccdd58509a746700e43e1c4ff8707a5ed0
        ;;
    *) echo "Build the macOS app on an Apple Silicon or Intel Mac." >&2; exit 1 ;;
esac

rg_path="resources/darwin-$arch/bin/rg"
icon_sha=f4e8e48e12a208975381865fde3a420e287079b14a1a0312908b4fb3f048994a
mkdir -p build "$(dirname "$rg_path")" assets
scratch=$(mktemp -d build/.desktop-resources.XXXXXX)
trap 'rm -rf "$scratch"' EXIT

verify() {
    printf '%s  %s\n' "$1" "$2" | shasum -a 256 -c -
}
download() {
    curl --fail --location --silent --show-error --retry 2 --proto '=https' -o "$2" "$1"
}

if [ ! -f "$rg_path" ]; then
    package="ripgrep-15.2.0-$target"
    download "https://github.com/BurntSushi/ripgrep/releases/download/15.2.0/$package.tar.gz" "$scratch/rg.tar.gz"
    verify "$archive_sha" "$scratch/rg.tar.gz"
    tar -xOzf "$scratch/rg.tar.gz" "$package/rg" > "$scratch/rg"
    verify "$rg_sha" "$scratch/rg"
    chmod 755 "$scratch/rg"
    mv "$scratch/rg" "$rg_path"
fi
verify "$rg_sha" "$rg_path"

if [ ! -f assets/icon.png ]; then
    download "https://github.com/cointem/ccdp/releases/download/v0.2.0/ccdp-icon.png" "$scratch/icon.png"
    verify "$icon_sha" "$scratch/icon.png"
    mv "$scratch/icon.png" assets/icon.png
fi
verify "$icon_sha" assets/icon.png
go run github.com/egoist/mygo/cmd/mygo@v0.9.2 build -skip-dmg
