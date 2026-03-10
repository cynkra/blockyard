FROM rust:1.85-bookworm AS builder

WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY src/ src/
COPY migrations/ migrations/

RUN cargo build --release --locked

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates iptables \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /src/target/release/blockyard /usr/local/bin/blockyard

EXPOSE 8080

ENTRYPOINT ["blockyard"]
