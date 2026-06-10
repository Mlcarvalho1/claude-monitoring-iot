FROM golang:1.24-alpine AS builder
WORKDIR /app

# fetch dependencies before copying source — better layer cache
COPY main.go .
RUN go mod init claude-monitor-api \
    && go get github.com/bogdanfinn/tls-client \
    && CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /api .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /api /api
EXPOSE 8080
ENTRYPOINT ["/api"]
