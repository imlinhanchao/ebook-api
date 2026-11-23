FROM golang:1.24-bookworm AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/ebook-api ./cmd/ebook-api

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build /out/ebook-api /app/ebook-api
COPY --from=build /src/static /app/static

ENV EBOOK_ADDR=0.0.0.0:8091 \
    EBOOK_DATA_DIR=/app/data \
    EBOOK_STATIC_DIR=/app/static

EXPOSE 8091
VOLUME ["/app/data"]

ENTRYPOINT ["/app/ebook-api"]
