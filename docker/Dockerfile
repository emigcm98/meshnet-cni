FROM golang:1.26-bookworm AS build

WORKDIR /src
# The daemon links libpcap for the gRPC wires, so it is the one CGO binary.
RUN apt-get update -y && \
    apt-get install -y --no-install-recommends libpcap-dev && \
    rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY daemon/ daemon/
COPY plugin/ plugin/
COPY utils/ utils/
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o meshnet ./plugin && \
    CGO_ENABLED=1 go build -ldflags="-s -w" -o meshnetd ./daemon

FROM ubuntu:24.04
RUN apt-get update && \
    apt-get install -y --no-install-recommends libpcap0.8 && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /src/meshnet /src/meshnetd /
COPY docker/entrypoint.sh /entrypoint.sh
COPY LICENSE /
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
