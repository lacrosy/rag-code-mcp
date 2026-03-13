# Stage 1: Build Go binaries
FROM golang:1.24-alpine AS go-builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/rag-code-mcp ./cmd/rag-code-mcp
RUN CGO_ENABLED=0 go build -o /out/index-all ./cmd/index-all

# Stage 2: PHP bridge with nikic/php-parser
FROM php:8.4-cli-alpine AS php-builder
COPY --from=composer:latest /usr/bin/composer /usr/bin/composer
WORKDIR /build/php-bridge
COPY php-bridge/composer.json php-bridge/composer.lock ./
RUN composer install --no-dev --optimize-autoloader --no-scripts
COPY php-bridge/ ./

# Stage 3: Runtime
FROM php:8.4-cli-alpine AS runtime
RUN apk add --no-cache bash

COPY --from=go-builder /out/rag-code-mcp /usr/local/bin/
COPY --from=go-builder /out/index-all /usr/local/bin/
COPY --from=php-builder /build/php-bridge /opt/ragcode/php-bridge

# PHP must be available for Go bridge
ENV RAGCODE_PHP_BRIDGE=/opt/ragcode/php-bridge/parse.php
ENV PATH="/usr/local/bin:${PATH}"

ENTRYPOINT ["rag-code-mcp"]
