#!/bin/bash
# Run by the games-on-whales entrypoint once the session's compositor
# environment is set up. A RomM session (pkg/controllers/romm.go) sets
# $DIREWOLF_LAUNCH_COMMAND to a `retroarch -L <core> <rom>` command line.
set -e

source /opt/gow/bash-lib/utils.sh

CFG_DIR=$HOME/.config/retroarch
mkdir -p "$CFG_DIR"
cp -u /cfg/retroarch.cfg "$CFG_DIR/retroarch.cfg"

source /opt/gow/launch-comp.sh
if [ -n "${DIREWOLF_LAUNCH_COMMAND:-}" ]; then
  gow_log "Starting RetroArch: $DIREWOLF_LAUNCH_COMMAND"
  # Through a script: sway's exec line would re-split the ROM path's quoting.
  launcher /opt/direwolf/launch
else
  gow_log "Starting RetroArch"
  launcher /usr/bin/retroarch
fi
