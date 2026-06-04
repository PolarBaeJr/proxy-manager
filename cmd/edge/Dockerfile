FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY internal/ ./internal/
COPY cmd/edge/ ./cmd/edge/
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o /edge ./cmd/edge

FROM gcr.io/distroless/static
COPY --from=build /edge /edge
ENTRYPOINT ["/edge"]
