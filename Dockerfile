# Copyright 2021 Jeffrey M Hodges.
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.17-buster@sha256:cefedeae41e0bbbfa20bb1c37c3a43e0001fa541be9732f7bc6a28ecc154e9e4 as build

WORKDIR /go/src/github.com/jmhodges/lekube
ADD . /go/src/github.com/jmhodges/lekube

RUN go build -o /go/bin/lekube

# Now copy it into our base image.
FROM gcr.io/distroless/base-debian10@sha256:40ff1808cc2cf2cab1a9c713eba49e21eab42fc7893bd5fb9ed81d8641df0771
COPY --from=build /go/bin/lekube /
CMD ["/lekube", "-conf", "/etc/lekube/lekube.json"]