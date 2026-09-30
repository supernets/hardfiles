FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o hardfiles main.go

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /build/hardfiles .
COPY www/ ./www/
COPY config.toml .
RUN mkdir -p files backgrounds
EXPOSE 5000
CMD ["./hardfiles"]
