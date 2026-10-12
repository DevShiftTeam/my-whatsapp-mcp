# Optional provisioning image; not a default archive dependency.
# Override TRANSCRIPTION_BASE with a reviewed digest before distributing an image.
ARG TRANSCRIPTION_BASE=python:3.11.14-slim-bookworm
FROM ${TRANSCRIPTION_BASE}
ENV PYTHONDONTWRITEBYTECODE=1
COPY services/transcription/ /opt/transcription/
RUN mkdir -p /var/lib/transcription && chmod 0700 /var/lib/transcription && chown 10001:10001 /var/lib/transcription
USER 10001:10001
VOLUME ["/var/lib/transcription"]
ENTRYPOINT []
CMD ["/var/lib/transcription/runtime/venv/bin/python", "/opt/transcription/server.py", "--env-file", "/var/lib/transcription/runtime/transcription.env"]
