FROM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS gosu
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOBIN=/out
# gosu 1.20 release commit. The Go checksum database verifies the source and
# go install retains its module version for vulnerability scanners.
RUN go install -trimpath -buildvcs=false github.com/tianon/gosu@v0.0.0-20261025094355-29b939aa5bd4

FROM postgres:18.6-alpine3.24@sha256:123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395
# The pinned base predates Alpine's fixes for krb5-libs and libxslt (HIGH findings published 2026-11-10).
# Require at least the fixed versions until a rebuilt base image carries them; then drop this line.
RUN apk add --no-cache --upgrade 'krb5-libs>=1.22.3-r0' 'libxslt>=1.1.43-r4'
COPY --from=gosu /out/gosu /usr/local/bin/gosu
ENV GOSU_VERSION=1.20
RUN gosu --version && test "$(gosu postgres id -u)" = "$(id -u postgres)"
