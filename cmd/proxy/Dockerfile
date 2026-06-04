FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY internal/ ./internal/
COPY cmd/proxy/ ./cmd/proxy/
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o /proxy ./cmd/proxy

FROM gcr.io/distroless/static
COPY --from=build /proxy /proxy
ENTRYPOINT ["/proxy"]
