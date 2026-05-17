# --- Build Stage ---
FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /app/server ./cmd/server

# --- Final Stage ---
FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/server /app/server

# 使用生产配置，本地 config.yaml 不受影响
RUN mkdir -p /app/config
COPY config/config.prod.yaml /app/config/config.yaml

EXPOSE 8081

ENTRYPOINT ["/app/server"]
