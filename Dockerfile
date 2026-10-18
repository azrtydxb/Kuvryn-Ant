# syntax=docker/dockerfile:1
ARG NODE_IMAGE=node:22.22.3-bookworm-slim
ARG GO_IMAGE=golang:1.27.2-bookworm
ARG RUNTIME_IMAGE=alpine:3.23
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS console
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY api /src/api
COPY web /src/web
RUN npm run build

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=console /src/internal/console/dist ./internal/console/dist
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w' -o /out/ant ./cmd/ant

FROM ${RUNTIME_IMAGE}
RUN apk add --no-cache ca-certificates && addgroup -g 10001 ant && adduser -D -H -u 10001 -G ant ant \
    && mkdir -p /var/lib/ant && chown 10001:10001 /var/lib/ant
COPY --from=build /out/ant /usr/local/bin/ant
COPY --chmod=0555 deploy/entrypoint.sh /usr/local/bin/ant-entrypoint
USER 10001:10001
ENV ANT_LISTEN=0.0.0.0:8080 ANT_DATA_DIR=/var/lib/ant/data
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/ant-entrypoint"]
CMD ["serve"]
