# The scenario driver: bash 5 and the command-line tools the scenarios use, with the docker CLI
# so a scenario can stop, start and kill the harness's own containers.
FROM alpine:3.22
RUN apk add --no-cache bash coreutils curl jq openssl postgresql17-client redis \
      docker-cli docker-cli-compose grep sed findutils util-linux
ENTRYPOINT ["/bin/bash"]
