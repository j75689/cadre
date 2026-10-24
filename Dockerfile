# One image per command: make image CMD=cadre-server, or
# DOCKER_BUILDKIT=1 docker build --build-arg CMD=cadre-server . (RUN --mount
# needs BuildKit).
# CMD is cadre-server, cadre-runner or cadre. The build stage runs on the
# builder's platform and cross-compiles, so a multi-arch build needs no
# emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build
ARG CMD
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    { test -n "$CMD" && test -d "cmd/$CMD" || { echo "CMD must be one of: $(ls cmd | tr "\n" " ")" >&2; exit 1; }; } && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/app "./cmd/$CMD"

# distroless/static has CA certificates and no shell. The user is given by
# number, nonroot's: a pod with runAsNonRoot cannot check a user by name.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
