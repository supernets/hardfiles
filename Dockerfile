FROM golang:1.21-alpine as builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN go build -o hardfiles main.go

FROM golang:1.21-alpine as app
WORKDIR /app
COPY --from=builder /build/hardfiles .
RUN mkdir files
CMD ["./hardfiles"]