FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w -X main.versionStr=${VERSION}" \
    -o /out/arrarr ./cmd/arrarr

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/arrarr /arrarr
EXPOSE 8080
USER nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/arrarr", "healthcheck"]
ENTRYPOINT ["/arrarr"]
