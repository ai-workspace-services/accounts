# ------------------------------
# Stage 1 — Build
# ------------------------------
FROM golang:1.25.1 AS builder

ARG GOPROXY
ENV GOPROXY=${GOPROXY}

WORKDIR /src

# 先复制 go.mod / go.sum，使 Docker 构建缓存层可复用
COPY go.mod go.sum ./
RUN go mod download

# 再复制源码
COPY . .

# 编译
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o account ./cmd/accountsvc/main.go
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o migratectl ./cmd/migratectl

# ------------------------------
# Stage 2 — Runtime
# ------------------------------
FROM ubuntu:24.04

WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates stunnel4 gettext-base netcat-openbsd curl \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/run/stunnel \
    && chown -R nobody:nogroup /var/run/stunnel

COPY --from=builder /src/account /usr/local/bin/account
COPY --from=builder /src/migratectl /usr/local/bin/migratectl
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
COPY config /app/config

RUN chmod +x /usr/local/bin/entrypoint.sh

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
