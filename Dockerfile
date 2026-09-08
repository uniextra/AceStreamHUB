FROM python:3.10-slim

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . .

EXPOSE 5004

CMD ["sh", "-c", "gunicorn --bind 0.0.0.0:${WEB_PORT:-5004} --workers 1 --threads 16 --timeout 120 app:app"]
