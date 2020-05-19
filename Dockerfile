FROM golang:alpine AS builder

# Define a working directory.
WORKDIR $GOPATH/powerdns_exporter/

# Copy the source in.
COPY . .

RUN set -ex \
    && go build -v -o /usr/local/bin/powerdns_exporter github.com/SeanWallace/powerdns_exporter


FROM alpine
MAINTAINER Sean Wallace <Sean@asgardweb.com>
EXPOSE 9120

ENV LISTEN_ADDRESS=":9120"
ENV METRIC_PATH="/metrics"
ENV API_URL="http://localhost:8001/"
ENV API_KEY=""

COPY --from=builder /usr/local/bin/powerdns_exporter /usr/local/bin

CMD ["sh", "-c", "powerdns_exporter"]
