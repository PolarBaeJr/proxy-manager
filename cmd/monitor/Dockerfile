FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum* ./
COPY internal/ ./internal/
COPY cmd/monitor/ ./cmd/monitor/
RUN go mod tidy && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /monitor ./cmd/monitor

FROM gcr.io/distroless/static
COPY --from=build /monitor /monitor
ENTRYPOINT ["/monitor"]
