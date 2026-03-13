# PHP Bridge — HTTP microservice for parsing PHP code via nikic/php-parser.
# Mounts project at /workspace, listens on port 9100.
#
# Build:  docker build -t ragcode-php-bridge ./php-bridge
# Run:    docker run -d -p 9100:9100 -v /path/to/project:/workspace:ro ragcode-php-bridge

FROM php:8.4-cli-alpine

COPY --from=composer:latest /usr/bin/composer /usr/bin/composer

WORKDIR /app

# Install dependencies first (layer caching)
COPY composer.json composer.lock ./
RUN composer install --no-dev --optimize-autoloader --no-scripts --quiet

# Copy source
COPY . .

EXPOSE 9100

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
  CMD wget -q --spider http://localhost:9100/health || exit 1

CMD ["php", "-S", "0.0.0.0:9100", "server.php"]
