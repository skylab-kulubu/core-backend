FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/stubs .

FROM alpine:3
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 stub
COPY --from=build /out/stubs /usr/local/bin/stubs
USER stub
ENTRYPOINT ["/usr/local/bin/stubs"]
