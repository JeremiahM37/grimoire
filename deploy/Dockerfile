# Grimoire Notes — container image
#   docker build -t grimoire-notes -f deploy/Dockerfile .
#   docker run -p 9111:9111 -v grimoire-vault:/vault grimoire-notes
#
# Configuration is entirely env-driven (see README "Config"): GRIMOIRE_VAULT,
# GRIMOIRE_PORT/HOST, GRIMOIRE_AUTH_TOKEN, GRIMOIRE_OLLAMA_URL, GRIMOIRE_SYNC_*…
FROM python:3.12-slim

WORKDIR /app

# runtime deps only (argon2-cffi is required by the secret vault)
RUN pip install --no-cache-dir \
    fastapi uvicorn watchdog cryptography python-multipart argon2-cffi

COPY server ./server
COPY web ./web
COPY cli ./cli
COPY plugins ./plugins

# run as a non-root user; the vault volume is the only writable path it needs
RUN useradd --create-home --uid 1000 grimoire \
    && mkdir /vault && chown grimoire:grimoire /vault
USER grimoire

ENV GRIMOIRE_VAULT=/vault \
    GRIMOIRE_PORT=9111 \
    GRIMOIRE_HOST=0.0.0.0
VOLUME /vault
EXPOSE 9111

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD ["python", "-c", "import urllib.request,os;urllib.request.urlopen(f'http://127.0.0.1:{os.environ.get(\"GRIMOIRE_PORT\",\"9111\")}/api/health')"]

CMD ["python", "-m", "server"]
