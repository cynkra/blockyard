FROM ghcr.io/rocker-org/r-ver:4.4.3

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       zlib1g-dev libssl-dev libcurl4-openssl-dev \
    && rm -rf /var/lib/apt/lists/*
