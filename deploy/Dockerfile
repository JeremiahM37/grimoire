FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir fastapi uvicorn httpx watchdog cryptography python-multipart
COPY server ./server
COPY web ./web
COPY cli ./cli
ENV MNEMO_VAULT=/vault MNEMO_PORT=9111
VOLUME /vault
EXPOSE 9111
CMD ["python", "-m", "server"]
