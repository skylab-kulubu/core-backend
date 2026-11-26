# pdf-tools: core's internal PDF service (README.md). Build context: this
# directory. Three stages: base (the service), test (base + the tests, what
# CI runs) and the last one, the image that runs.
#
# It runs as a non-root user (10001) that can write only to /tmp; everything
# else in the image is root's. Run it with a read-only root filesystem and a
# tmpfs /tmp (README.md lists the settings).
FROM --platform=linux/amd64 python:3.12-slim-trixie AS base
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-deps --require-hashes -r requirements.txt
COPY pdf_tools ./pdf_tools
RUN python -m compileall -q pdf_tools \
 && useradd --uid 10001 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin pdftools

FROM base AS test
COPY requirements-test.txt ./
RUN pip install --no-deps --require-hashes -r requirements-test.txt
COPY tests ./tests
USER 10001:10001
ENV TMPDIR=/tmp
CMD ["python", "-m", "pytest", "-q", "-p", "no:cacheprovider", "tests"]

FROM base
LABEL org.opencontainers.image.source="https://github.com/skylab-kulubu/core-backend" \
      org.opencontainers.image.description="core-backend PDF tools (inspect, strip-text, place, render, font sanitize); internal network only"
RUN pip uninstall -y pip
USER 10001:10001
ENV PORT=8080 TMPDIR=/tmp
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["python", "-m", "pdf_tools", "health"]
ENTRYPOINT ["python", "-m", "pdf_tools"]
CMD ["serve"]
