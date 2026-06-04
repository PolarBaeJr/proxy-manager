FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -ldflags="-s -w" -o /dashboard .

FROM gcr.io/distroless/static
COPY --from=build /dashboard /dashboard
ENTRYPOINT ["/dashboard"]
