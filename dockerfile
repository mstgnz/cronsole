# Build.
FROM golang:1.25-alpine AS builder

ARG VERSION=dev

WORKDIR /app

# Dependencies are copied and downloaded on their own so the layer is reused
# whenever only source has changed.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Templates and static assets are embedded in the binary, so the image needs
# nothing beside it. A container built without its assets directory would
# otherwise fail on the first page rather than at build time.
RUN GOOS=linux CGO_ENABLED=0 go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /cronsole ./cmd/cronsole

# Run.
FROM alpine:3.20

# ca-certificates is needed to call an https job target at all. tzdata is here
# as well as compiled in, so the container's own clock reads the same zone.
RUN apk add --no-cache ca-certificates tzdata

# An unprivileged account. The service opens one port and talks to Postgres;
# nothing it does needs root, and running as root turns a remote code flaw into
# a container takeover.
RUN adduser -D -u 10001 cronsole

COPY --from=builder /cronsole /usr/local/bin/cronsole

USER cronsole
ENV TZ=Europe/Istanbul
EXPOSE 3333

# The liveness endpoint touches nothing but the process. A health check that
# queries the database restarts the container every time the database hiccups,
# which turns a brief outage into a restart loop.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:${APP_PORT:-3333}/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/cronsole"]
