FROM ghcr.io/games-on-whales/base-app:edge

ARG TARGETARCH
ARG TARGETOS

RUN apt-get update && apt-get install -y python3-venv curl
RUN python3 -m venv /app/venv
RUN /app/venv/bin/pip install --no-cache-dir --upgrade pip
RUN /app/venv/bin/pip install --no-cache-dir --upgrade requests-unixsocket2

COPY --chown=ubuntu:ubuntu startup.sh /opt/gow/startup.sh
COPY --chown=ubuntu:ubuntu script.py /app/wolf-agent.py
COPY --chown=ubuntu:ubuntu bin/wolf-agent-${TARGETOS}-${TARGETARCH} /app/wolf-agent

RUN chmod +x /app/wolf-agent
RUN chmod +x /opt/gow/startup.sh

ENV PATH="/app/venv/bin:${PATH}"
ENTRYPOINT ["/entrypoint.sh"]
