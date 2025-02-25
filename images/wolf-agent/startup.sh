#!/usr/bin/env bash

#! Wait for wolf socket to appear
# WOLF_SOCKET_PATH=${WOLF_SOCKET_PATH:-/tmp/wolf.sock}
# while [ ! -S $WOLF_SOCKET_PATH ]; do
#   sleep 1
# done

#! Start wolf agent
# echo "Starting legacy python script" >&2
# python3 /app/wolf-agent.py $WOLF_SOCKET_PATH

echo "Starting wolf agent" >&2
exec /app/wolf-agent --socket $WOLF_SOCKET_PATH --port 8443

# curl -N --unix-socket /etc/wolf/wolf.sock http://localhost/api/v1/events
