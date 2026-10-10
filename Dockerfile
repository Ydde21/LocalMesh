# LocalMesh control plane.
#
# Multi-stage: build the Go binary, then run it in a minimal image.
# TLS certs and the internal CA are generated at first boot into the
# data directory (LOCALMESH_DATA_DIR, default /data — mount a volume).

FROM golang:1.27-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/localmesh-server ./cmd/localmesh-server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 localmesh \
    && mkdir -p /data && chown localmesh:localmesh /data
COPY --from=build /out/localmesh-server /usr/local/bin/localmesh-server
USER localmesh
VOLUME /data
ENV LOCALMESH_DATA_DIR=/data LOCALMESH_LISTEN=0.0.0.0:8443
EXPOSE 8443
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s \
  CMD wget -q --no-check-certificate -O /dev/null https://127.0.0.1:8443/healthz || exit 1
ENTRYPOINT ["localmesh-server"]
