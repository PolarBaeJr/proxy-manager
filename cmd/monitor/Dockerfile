FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /monitor .

FROM gcr.io/distroless/static
COPY --from=build /monitor /monitor
ENTRYPOINT ["/monitor"]
