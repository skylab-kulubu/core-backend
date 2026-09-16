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
CMD ["./core-backend"]
