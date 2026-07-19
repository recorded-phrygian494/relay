# Distroless static image (DESIGN §12 phase 5). CGO_ENABLED=0 everywhere —
# modernc.org/sqlite keeps the request log pure Go.
#
# Build:  docker build -t relay .
# Run:    docker run -p 4000:4000 -e OPENAI_API_KEY -e RELAY_API_KEY \
#           -v relay-data:/data ghcr.io/llmrelay/relay \
#           serve --listen 0.0.0.0:4000 --db /data/relay.db
# Note: a non-loopback listen REQUIRES server.api_keys (or RELAY_API_KEY in
# your relay.yaml) — relay refuses to start as an open proxy.

FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /relay ./cmd/relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /relay /relay
VOLUME /data
EXPOSE 4000
ENTRYPOINT ["/relay"]
CMD ["serve"]
