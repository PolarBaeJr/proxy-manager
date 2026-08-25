FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG BUILD_VERSION=dev
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum* ./
COPY internal/ ./internal/
COPY cmd/dashboard/ ./cmd/dashboard/
RUN go mod tidy && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" -o /dashboard ./cmd/dashboard

FROM gcr.io/distroless/static
COPY --from=build /dashboard /dashboard
ENTRYPOINT ["/dashboard"]
