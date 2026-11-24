# Vedetta core image (map R8): only Vedetta. Search, reading services and the model
# are declared by address in the first run or in .env; see deploy/.
#
# Build:  docker build -t vedetta .   (podman build works the same)

# The compiler runs on the build machine and cross-compiles: Vedetta is pure Go.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$(cat VERSION)" -o /out/vedetta . \
 && mkdir -p /out/data

# No shell, no package manager: certificates, time zones, a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
WORKDIR /app
COPY --from=build /out/vedetta /app/vedetta
COPY --from=build --chown=65532:65532 /out/data /app/data
# Inside the container Vedetta listens on every interface; the compose decides who
# reaches it (VEDETTA_PUBLISH, 127.0.0.1 by default).
ENV VEDETTA_BIND=0.0.0.0
EXPOSE 3030
VOLUME /app/data
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD ["/app/vedetta", "health"]
ENTRYPOINT ["/app/vedetta"]
