FROM golang:1.21-alpine
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN go build -o hardfiles main.go
RUN mkdir files
CMD ["./hardfiles"]