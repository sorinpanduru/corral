# PHP-FPM with the MongoDB extension, for the corral functional tests
# (ADR-0022). The base image is pinned by digest and the extension by exact
# version; both are passed in by the Go fixture from its constants.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
ARG PECL_MONGODB_VERSION
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends $PHPIZE_DEPS libssl-dev libsasl2-dev libicu-dev; \
    pecl install "mongodb-${PECL_MONGODB_VERSION}"; \
    docker-php-ext-enable mongodb; \
    apt-get purge -y --auto-remove $PHPIZE_DEPS libssl-dev libsasl2-dev libicu-dev; \
    rm -rf /var/lib/apt/lists/* /tmp/pear
COPY php-fpm.conf /usr/local/etc/php-fpm.d/zzz-corral.conf
RUN mkdir -p /srv/scripts
EXPOSE 9000
