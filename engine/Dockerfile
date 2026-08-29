# syntax=docker/dockerfile:1
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/continuum ./cmd/continuum

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git \
  && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/continuum /usr/local/bin/continuum
RUN mkdir -p /var/lib/continuum
EXPOSE 8080
ENTRYPOINT ["continuum"]
CMD ["serve"]
