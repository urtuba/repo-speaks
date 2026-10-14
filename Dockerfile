# Repo Speaks in one image: the server and its commands.
# Build it with: docker build -t repo-speaks .

# The Go version matches go.mod.
FROM golang:1.27.2-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The SQLite driver is pure Go, so the binary needs no C library.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/repo-speaks .

# git syncs the repositories; Debian 12 ships git 2.39, and the server
# needs 2.32 or newer. ca-certificates lets it check HTTPS hosts.
FROM debian:bookworm-slim
RUN apt-get update \
	&& apt-get install -y --no-install-recommends git ca-certificates \
	&& rm -rf /var/lib/apt/lists/*

# A fixed user ID, so the files in the data volume keep their owner
# across new images. Only this user can read the data folder.
RUN groupadd --system --gid 10001 repo-speaks \
	&& useradd --system --uid 10001 --gid 10001 --home-dir /data --no-create-home --shell /usr/sbin/nologin repo-speaks \
	&& mkdir /data \
	&& chown 10001:10001 /data \
	&& chmod 700 /data

COPY --from=build /out/repo-speaks /usr/local/bin/repo-speaks

USER 10001:10001
ENV REPO_SPEAKS_DATA_DIR=/data \
	REPO_SPEAKS_ADDR=:8080
VOLUME /data
EXPOSE 8080

# The image has no curl, so bash asks /healthz itself.
HEALTHCHECK --interval=1m --timeout=5s --start-period=10s --retries=3 \
	CMD ["bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/8080 && printf 'GET /healthz HTTP/1.0\\r\\nHost: localhost\\r\\n\\r\\n' >&3 && head -n 1 <&3 | grep -q ' 200 '"]

CMD ["repo-speaks"]
