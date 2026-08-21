# Build the tofu-runner image used by TofuModule controller Jobs.
#
#   docker build -t tofu-runner:latest -f Dockerfile .
#   or via Makefile:  make docker-build-runner

FROM alpine:3.20

ARG TOFU_VERSION=1.12.6

RUN apk add --no-cache \
      bash \
      ca-certificates \
      curl \
      git \
      jq \
      kubectl \
    && curl -fsSL \
      "https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION}/tofu_${TOFU_VERSION}_linux_amd64.zip" \
      -o /tmp/tofu.zip \
    && unzip /tmp/tofu.zip -d /usr/local/bin \
    && rm /tmp/tofu.zip \
    && tofu version

COPY runner/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
