FROM hugomods/hugo:exts-0.154.5 AS docs
WORKDIR /docs
COPY docs/ .
RUN hugo --minify --baseURL /docs/ --enableGitInfo=false

FROM node:25-alpine AS css-builder
WORKDIR /src/internal/ui
COPY internal/ui/package.json internal/ui/package-lock.json ./
RUN npm ci
COPY internal/ui/input.css ./
COPY internal/ui/templates/ templates/
RUN npm run css:build

FROM golang:1.26.0-alpine AS builder

ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=docs /docs/public internal/docs/dist
COPY --from=css-builder /src/internal/ui/static/style.css internal/ui/static/style.css

ARG COVER=""
ARG VERSION=dev
# Docker-only variant: phase 3-8 build-tag split. The minimal mode
# switch flips default-include-all off, and docker_backend opts the
# Docker implementation back in. The resulting binary does not
# import internal/backend/process and cannot speak to a process
# backend config.
RUN CGO_ENABLED=0 go build ${COVER:+-cover} \
    -tags "minimal,docker_backend" \
    -ldflags "-X main.version=${VERSION}" \
    -o /blockyard ./cmd/blockyard
RUN CGO_ENABLED=0 go build ${COVER:+-cover} -o /by-builder ./cmd/by-builder

FROM alpine:3.23

RUN apk add --no-cache ca-certificates curl iptables

COPY --from=builder /blockyard /usr/local/bin/blockyard
COPY --from=builder /by-builder /usr/local/lib/blockyard/by-builder
COPY blockyard.toml /etc/blockyard/blockyard.toml

EXPOSE 8080

ENTRYPOINT ["blockyard", "--config", "/etc/blockyard/blockyard.toml"]
