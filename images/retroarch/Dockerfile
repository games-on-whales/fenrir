# RetroArch for RomM sessions: games-on-whales' RetroArch image (Wolf's
# compositor setup) plus what a session needs to start straight into one ROM
# ($DIREWOLF_LAUNCH_COMMAND), none of it downloaded at session start: the
# libretro cores the operator launches ROMs with (retroArchPlatforms in
# pkg/controllers/romm.go), controller profiles and menu assets, all from
# libretro's stable build, pinned by checksum.
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS libretro

ARG RETROARCH_VERSION=1.22.2
# Must be updated by hand with the version: the build fails closed rather
# than installing unverified cores.
ARG RETROARCH_SHA256=7d62da9a21397d6e1b9490785cedbeafd262781b50115076736fbe8a77ef30e9
ARG CORES_SHA256=4b7ed8dc97d4bf035fce182c64b5658c7662e2e9e5d42129538afbd4b6096307
# Every core retroArchPlatforms names (TestRetroArchImageHasEveryCore).
ARG CORES="atari800 fbneo fceumm gambatte genesis_plus_gx mednafen_pce_fast melondsds mgba mupen64plus_next pcsx_rearmed picodrive snes9x stella yabasanshiro"

RUN set -eu && \
  apk add --no-cache 7zip curl && \
  base="https://buildbot.libretro.com/stable/${RETROARCH_VERSION}/linux/x86_64" && \
  home="RetroArch-Linux-x86_64/RetroArch-Linux-x86_64.AppImage.home/.config/retroarch" && \
  curl -fsSL -o /tmp/retroarch.7z "${base}/RetroArch.7z" && \
  curl -fsSL -o /tmp/cores.7z "${base}/RetroArch_cores.7z" && \
  printf '%s  %s\n' "${RETROARCH_SHA256}" /tmp/retroarch.7z "${CORES_SHA256}" /tmp/cores.7z | sha256sum -c - && \
  mkdir -p /out/cores /out/share && \
  for c in ${CORES}; do set -- "$@" "${home}/cores/${c}_libretro.so"; done && \
  7z e -bd -o/out/cores /tmp/cores.7z "$@" && \
  for c in ${CORES}; do test -f "/out/cores/${c}_libretro.so"; done && \
  7z x -bd -o/tmp/ra /tmp/retroarch.7z "${home}/assets" "${home}/autoconfig" && \
  mv "/tmp/ra/${home}/assets" "/tmp/ra/${home}/autoconfig" /out/share/ && \
  test -d /out/share/autoconfig/udev

FROM ghcr.io/games-on-whales/retroarch:edge@sha256:bbcf4523e589fc7177b522ce56ba9507c6530caaf1999e37b37062a189f18cf2

COPY --from=libretro /out/cores/ /usr/lib/x86_64-linux-gnu/libretro/
COPY --from=libretro /out/share/ /usr/share/libretro/
# Replaces the base image's startup-app.sh (which downloads assets and
# controller profiles into the session's empty home on every start).
COPY --chmod=755 root/ /

RUN set -eu && \
  printf '%s\n' \
    'libretro_directory = "/usr/lib/x86_64-linux-gnu/libretro"' \
    'assets_directory = "/usr/share/libretro/assets"' \
    'joypad_autoconfig_dir = "/usr/share/libretro/autoconfig"' \
    >> /cfg/retroarch.cfg
