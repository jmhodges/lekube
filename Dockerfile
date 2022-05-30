# Copyright 2021 Jeffrey M Hodges.
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.18.2-buster@sha256:ad5d8648c50bd3b2dfdb3bd4f6ab52a82b43d949f53c1998884b1653e0b873b5 as build

WORKDIR /go/src/github.com/jmhodges/lekube
ADD . /go/src/github.com/jmhodges/lekube

RUN go build -o /go/bin/lekube

# Now copy it into our base image.
FROM gcr.io/distroless/base-debian10@sha256:d8244d4756b5dc43f2c198bf4e37e6f8a017f13fdd7f6f64ec7ac7228d3b191e
COPY --from=build /go/bin/lekube /
CMD ["/lekube", "-conf", "/etc/lekube/lekube.json"]