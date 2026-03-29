FROM golang:1.25.8-bookworm AS builder

ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG COVER=""
RUN CGO_ENABLED=0 go build ${COVER:+-cover} -o /blockyard ./cmd/blockyard
RUN CGO_ENABLED=0 go build ${COVER:+-cover} -o /by-builder ./cmd/by-builder

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates curl iptables \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /blockyard /usr/local/bin/blockyard
COPY --from=builder /by-builder /usr/local/lib/blockyard/by-builder
COPY blockyard.toml /etc/blockyard/blockyard.toml

EXPOSE 8080

ENTRYPOINT ["blockyard", "--config", "/etc/blockyard/blockyard.toml"]
