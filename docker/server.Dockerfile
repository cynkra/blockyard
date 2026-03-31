FROM golang:1.25.8-alpine AS builder

ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG COVER=""
ARG VERSION=dev
RUN CGO_ENABLED=0 go build ${COVER:+-cover} -ldflags "-X main.version=${VERSION}" -o /blockyard ./cmd/blockyard
RUN CGO_ENABLED=0 go build ${COVER:+-cover} -o /by-builder ./cmd/by-builder

FROM alpine:3.23

RUN apk add --no-cache ca-certificates curl iptables

COPY --from=builder /blockyard /usr/local/bin/blockyard
COPY --from=builder /by-builder /usr/local/lib/blockyard/by-builder
COPY blockyard.toml /etc/blockyard/blockyard.toml

EXPOSE 8080

ENTRYPOINT ["blockyard", "--config", "/etc/blockyard/blockyard.toml"]
