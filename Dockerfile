FROM --platform=$BUILDPLATFORM golang:1.24rc1-alpine AS build

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY schema.sql .
COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -o /app/main

FROM gcr.io/distroless/static:nonroot AS app
COPY --from=build /app/main /main
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 8788
CMD ["/main"]
