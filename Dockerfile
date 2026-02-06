FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o fastregistry ./cmd/fastregistry

FROM alpine:latest

RUN apk add --no-cache ca-certificates coreos-installer

COPY --from=builder /app/fastregistry /usr/local/bin/fastregistry

EXPOSE 5000

ENTRYPOINT ["/usr/local/bin/fastregistry"]
CMD ["-config", "/etc/fastregistry/config.yaml"]
