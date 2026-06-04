FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY internal/ ./internal/
COPY cmd/monitor/ ./cmd/monitor/
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o /monitor ./cmd/monitor

FROM gcr.io/distroless/static
COPY --from=build /monitor /monitor
ENTRYPOINT ["/monitor"]
