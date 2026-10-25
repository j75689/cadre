# One image per command: make image CMD=cadre-server, or
# DOCKER_BUILDKIT=1 docker build --build-arg CMD=cadre-server . (RUN --mount
# needs BuildKit).
# CMD is cadre-server, cadre-runner or cadre. The build stages run on the
# builder's platform and cross-compile, so a multi-arch build needs no
# emulation. Only cadre-server carries the web UI: the ui stage is chosen by
# CMD, and BuildKit builds the Node stage only for it. A CMD that is
# missing or names no command fails there: "ui-<CMD>: failed to resolve
# source metadata" (an image of that name is looked for and not found).
ARG CMD

FROM --platform=$BUILDPLATFORM node:24 AS ui-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY web/ ./
RUN npm run build

FROM scratch AS ui-cadre-server
COPY --from=ui-build /src/internal/web/dist /dist
FROM scratch AS ui-cadre-runner
FROM scratch AS ui-cadre
FROM ui-${CMD} AS ui

FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=ui / /src/internal/web/
# Declared here, not above: a RUN after an ARG is cached by its value, and
# the version differs on every build.
ARG CMD
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/app "./cmd/$CMD"

# distroless/static has CA certificates and no shell. The user is given by
# number, nonroot's: a pod with runAsNonRoot cannot check a user by name.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
