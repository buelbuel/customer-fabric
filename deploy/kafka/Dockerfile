FROM apache/kafka:4.3.2@sha256:852a0d49fc4e656b022e7f23785411802c9a088c78a7c5e39f33969de9c227a7
# The pinned image predates Alpine's fixes for brotli-libs (CRITICAL), libexpat and musl (HIGH). Require
# at least the fixed versions until a rebuilt upstream image carries them; then use it directly.
USER root
RUN apk add --no-cache --upgrade 'brotli-libs>=1.2.1-r0' 'libexpat>=2.10.0-r0' 'musl>=1.2.6-r3' 'musl-utils>=1.2.6-r3'
USER appuser
