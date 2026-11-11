FROM python:3.15-slim
RUN pip install --no-cache-dir "mcp>=1.12,<2" "httpx>=0.27" "uvicorn>=0.30"
WORKDIR /app
COPY server.py .
USER 10001
CMD ["python", "server.py"]
