FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY daemon/ daemon/
COPY plugin/ plugin/
COPY utils/ utils/
ENV CGO_ENABLED=0
RUN go build -ldflags="-s -w" -o meshnet ./plugin && \
    go build -ldflags="-s -w" -o meshnetd ./daemon

FROM busybox:1.37.0
COPY --from=build /src/meshnet /src/meshnetd /
COPY docker/entrypoint.sh /entrypoint.sh
COPY LICENSE /
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
