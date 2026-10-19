# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go tool templ generate && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cryptkeeper .

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/cryptkeeper /app/cryptkeeper
# config.yaml, .env and data/ are mounted at runtime (see docker-compose.yml)
EXPOSE 8787
HEALTHCHECK --interval=60s --timeout=5s --start-period=60s --retries=3 CMD ["/app/cryptkeeper", "healthcheck"]
ENTRYPOINT ["/app/cryptkeeper"]
CMD ["serve", "-config", "/app/config.yaml", "-data", "/app/data", "-env", "/app/.env", "-addr", ":8787"]
