# hello-shiny

Minimal example that boots blockyard and deploys a Shiny app.

## Prerequisites

- Docker (with Compose v2)

## Usage

```bash
# Start blockyard (builds the server image on first run)
docker compose up -d --build

# Deploy the app
./deploy.sh

# Open in browser
open http://localhost:8080/app/hello-shiny/
```

## Cleanup

```bash
docker compose down -v
```
