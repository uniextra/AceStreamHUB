FROM python:3.10-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . .

# Expose port for the Flask app (HDHomeRun uses 5004 by default for streaming API)
EXPOSE 5004

CMD ["python", "app.py"]
