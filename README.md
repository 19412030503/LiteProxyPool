# LiteProxy

## Docker

```bash
docker build -t lite-proxy .
docker run --rm -p 1080:1080 -p 18080:18080 -p 8088:8088 lite-proxy
```
## Docker Compose

```bash
docker compose up -d --build
docker compose logs -f
docker compose down
```

```bash
docker run --rm -p 1080:1080 -p 18080:18080 -p 8088:8088 -v "$PWD/config.json:/app/config.json:ro" lite-proxy
```
