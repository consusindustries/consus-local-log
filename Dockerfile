# Two-stage build: compile a static binary, ship it alone on scratch.
# The final image contains the binary, the CA bundle it needs for outbound TLS,
# and an empty log directory. Nothing else to audit or patch.

FROM golang:1.26-alpine AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
# Explicit COPY (not `COPY . .`) so a reviewer sees exactly what enters the image.
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /consus-local-log .
# Staged so the final image carries a log directory the nonroot UID owns.
# Docker seeds a fresh volume from the image's directory, ownership included;
# without this the volume would be root-owned and the proxy would forward
# every request while silently logging none of them.
RUN mkdir -p /staging/logs

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /staging/logs /var/log/consus
COPY --from=build /consus-local-log /consus-local-log
# Numeric UID: scratch has no /etc/passwd. 65532 is the distroless "nonroot" convention.
USER 65532:65532
VOLUME /var/log/consus
EXPOSE 4000
ENTRYPOINT ["/consus-local-log"]
