# Copyright 2021 Jeffrey M Hodges.
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.25.3@sha256:6ea52a02734dd15e943286b048278da1e04eca196a564578d718c7720433dbbe as build

WORKDIR /go/src/github.com/jmhodges/lekube
ADD . /go/src/github.com/jmhodges/lekube

RUN go build -o /go/bin/lekube

# Now copy it into our base image.
FROM gcr.io/distroless/base-debian12
COPY --from=build /go/bin/lekube /
CMD ["/lekube", "-conf", "/etc/lekube/lekube.json"]
