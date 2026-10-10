# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# modernc.org/sqlite is pure Go, so the binary is fully static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/flagpost ./cmd/flagpost
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/flagpost /usr/local/bin/flagpost
COPY --from=build --chown=65532:65532 /out/data /data
ENV FLAGPOST_DB=/data/flagpost.db \
    FLAGPOST_LOG_FORMAT=json
# The server listens on $PORT when it is set (as on most PaaS platforms)
# and on 8080 otherwise.
EXPOSE 8080
VOLUME ["/data"]
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/flagpost"]
CMD ["serve"]
