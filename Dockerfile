# Build
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/api ./cmd/api

# Run
FROM alpine:3.21
RUN adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/api /app/api
COPY config/ /app/config/
# Non-root: the API holds the LLM keys and the CRM credentials, so it runs with
# the least privilege the container can give it (docs/04-ARCHITECTURE.md §5).
USER app
EXPOSE 8080
ENTRYPOINT ["/app/api"]
