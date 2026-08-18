FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum* ./
COPY internal/ ./internal/
COPY cmd/statusbot/ ./cmd/statusbot/
RUN go mod tidy && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /statusbot ./cmd/statusbot

FROM gcr.io/distroless/static
COPY --from=build /statusbot /statusbot
ENTRYPOINT ["/statusbot"]
