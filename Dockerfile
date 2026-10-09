FROM --platform=linux/amd64 golang:1.25-alpine AS builder
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/core-backend ./cmd/core-backend

FROM --platform=linux/amd64 alpine:latest
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /out/core-backend .
EXPOSE 8080
ENV PORT=8080
# The container's health check is core's readiness, asked by the binary
# itself (no curl or wget needed): GET /v1/ready?gate=skip answers 204 once
# core listens (migrations done) while its database answers and it is not
# shutting down; the access gate's Redis is left out
# (docs/health-and-shutdown.md). Swarm routes to a task only once it is
# healthy, so a start-first deploy moves traffic over when the new task can
# serve. start-period covers startup (migrations, the
# Keycloak role checks, the access gate's reconciliation); 6 retries 10 s
# apart let a database restart pass without a restart of core. A Health
# Check set on the Dokploy service replaces this one.
HEALTHCHECK --interval=10s --timeout=5s --start-period=120s --start-interval=2s --retries=6 \
  CMD ["/app/core-backend", "healthcheck"]
CMD ["./core-backend"]
