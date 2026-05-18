# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /app/server ./cmd/server

FROM alpine:latest

WORKDIR /app

RUN apk add --no-cache ca-certificates

COPY --from=builder /app/server /app/server
COPY config/config.docker.yaml /app/config/config.yaml

RUN mkdir -p /app/logs

EXPOSE 8080

ENTRYPOINT ["/app/server"]
