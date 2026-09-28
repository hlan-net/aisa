# aisa: a single static binary in a distroless image, for linux/amd64 and linux/arm64.
#
#   docker build -t aisa:local .
#   docker build --build-arg VERSION=v0.2.0 -t aisa:v0.2.0 .
#
# The build stage runs on the build platform and cross-compiles, so a multi-arch build needs no
# emulation for the compiler.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X github.com/hlan-net/aisa/internal/version.Version=$VERSION" \
      -o /out/aisa ./cmd/aisa

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/aisa /usr/local/bin/aisa
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD ["/usr/local/bin/aisa", "-healthcheck", "http://127.0.0.1:8080/healthz"]
ENTRYPOINT ["/usr/local/bin/aisa"]
