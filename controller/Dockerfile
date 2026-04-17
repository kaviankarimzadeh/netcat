# Built for linux/amd64 Kubernetes nodes. Use a native build stage on
# the build host (BUILDPLATFORM) and cross-compile the static binary for
# amd64 so builds stay fast on Apple Silicon / ARM dev machines too.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY controller/ ./controller/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -trimpath -o /out/netcat-controller ./controller

FROM --platform=linux/amd64 gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/netcat-controller /netcat-controller
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/netcat-controller"]
