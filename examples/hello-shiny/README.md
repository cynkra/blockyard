# hello-shiny

Minimal example that boots blockyard and deploys a Shiny app.

## Prerequisites

- Docker (with Compose v2)

## Usage

```bash
# Start blockyard
docker compose up -d

# Deploy the app
./deploy.sh

# Open in browser
open http://localhost:8080/app/hello-shiny/
```

## Cleanup

```bash
docker compose down -v
```
