# Game library desktop: linuxserver's Selkies Steam image plus Heroic Games
# Launcher (Epic/GOG/Amazon). Served over Selkies behind Authentik, so the
# built-in Selkies password (PASSWORD/CUSTOM_USER) is deliberately left unset.
# Run it with /dev/shm as a memory-backed emptyDir: Steam's CEF and Heroic's
# Electron both fall over on the 64Mi container default.
FROM lscr.io/linuxserver/steam:latest@sha256:d1c9f2c6d522169f6dcede9b17076c0e31ddbc5f9c4a102562cecddfe2d4ecdb

# renovate: datasource=github-releases depName=Heroic-Games-Launcher/HeroicGamesLauncher
ARG HEROIC_VERSION=v2.22.3
# Must be updated by hand with the version: a Renovate bump without it fails
# the build closed rather than installing an unverified .deb.
ARG HEROIC_SHA256=f89eed7e0eb900fbe3051edfedbe7532db14328241cbf8c1fb766462dfc0b849

# First, so the RUN below can point Heroic's own launchers at the wrapper.
COPY --chmod=755 root/ /

RUN \
  echo "**** install heroic ****" && \
  curl -fsSL -o /tmp/heroic.deb \
    "https://github.com/Heroic-Games-Launcher/HeroicGamesLauncher/releases/download/${HEROIC_VERSION}/Heroic-${HEROIC_VERSION#v}-linux-amd64.deb" && \
  echo "${HEROIC_SHA256}  /tmp/heroic.deb" | sha256sum -c - && \
  apt-get update && \
  apt-get install -y --no-install-recommends /tmp/heroic.deb && \
  echo "**** drop setuid chrome-sandbox ****" && \
  # postinst makes it setuid root or not depending on the build host; it is
  # never needed, as wrapped-heroic uses user namespaces or --no-sandbox.
  chmod 0755 /opt/Heroic/chrome-sandbox && \
  echo "**** route every heroic launcher through wrapped-heroic ****" && \
  update-alternatives --install /usr/bin/heroic heroic /usr/bin/wrapped-heroic 1000 && \
  sed -i 's|^Exec=/opt/Heroic/heroic|Exec=/usr/bin/wrapped-heroic|' /usr/share/applications/heroic.desktop && \
  ! grep -q '^Exec=/opt/Heroic' /usr/share/applications/heroic.desktop && \
  echo "**** add heroic to the openbox menus ****" && \
  for menu in /defaults/menu.xml /defaults/menu_wayland.xml; do \
    sed -i 's|</menu>|<item label="Heroic" icon="/usr/share/icons/hicolor/48x48/apps/heroic.png"><action name="Execute"><command>/usr/bin/wrapped-heroic</command></action></item>\n</menu>|' "$menu" && \
    grep -q wrapped-heroic "$menu" || exit 1; \
  done && \
  echo "**** cleanup ****" && \
  apt-get autoclean && \
  rm -rf \
    /var/lib/apt/lists/* \
    /var/tmp/* \
    /tmp/*
