FROM python:3.12-alpine
# Copy extras/wasip3/sdk/python/c2wbridge.py into this directory first.
COPY c2wbridge.py serve.py /app/
WORKDIR /app
CMD ["python3", "serve.py"]
