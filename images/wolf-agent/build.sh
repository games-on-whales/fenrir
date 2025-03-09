#!/usr/bin/env bash


DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd -P )"

TARGETS=(
    "games-on-whales.github.io/direwolf/cmd/wolf-agent"
)
PLATFORMS=("linux/amd64" "linux/arm64" "linux/arm" "darwin/arm64")

mkdir -p "$DIR/bin"

for TARGET in "${TARGETS[@]}"; do
    for PLATFORM in "${PLATFORMS[@]}"; do
        PLATFORM_SPLIT=(${PLATFORM//\// })
        OS=${PLATFORM_SPLIT[0]}
        ARCH=${PLATFORM_SPLIT[1]}
        echo "Building $TARGET for $OS/$ARCH" >&2
        GOOS=$OS GOARCH=$ARCH go build -o "$DIR/bin/$(basename $TARGET)-$OS-$ARCH" $TARGET
    done
done
