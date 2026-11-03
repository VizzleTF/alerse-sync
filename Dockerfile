# Compiles on the build machine for the target platform, so multi-arch
# images build without emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o /arcomium-sync ./cmd/arcomium-sync

# A static binary on distroless: no shell, no package manager, not root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /arcomium-sync /arcomium-sync
EXPOSE 8295
ENTRYPOINT ["/arcomium-sync"]
